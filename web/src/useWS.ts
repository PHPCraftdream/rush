import { useEffect } from "react";
import { ws, sendLoadMessages, isStaleMessagesReply, hasLiveEventsSinceRequest, bumpLiveEventEpoch, sendListSessions, isStaleSessionsListReply, takeSessionsLiveDelta, recordSessionLiveUpdate, recordSessionLiveDelete } from "./ws";
import {
  $connected,
  $config,
  $updateAvailable,
  $mcpState,
  $agentError,
  $sessions,
  $activeSessionID,
  $busySessions,
  $messageQueue,
  applyMessagesSnapshot,
  applySubAgentMessagesSnapshot,
  applySessionsSnapshot,
  setSessions,
  upsertSession,
  removeSession,
  removeSessionQueueAndBusy,
  upsertMessage,
  removeMessage,
  setSessionBusy,
  setActiveSession,
  $recentSmartModels,
  $recentFastModels,
  trackModelUsage,
  dequeueAllMessages,
  enqueueMessage,
  applyTheme,
  setSkills,
  setSummarizeQueued,
  registerSubAgentSession,
  isSubAgentSession,
  upsertSubAgentMessage,
  tombstoneMessage,
  removeSubAgentMessage,
  trackMessageParts,
} from "./store";
import type { WSMessage, Session, Message, ConfigPayload, MCPState, AgentBusyPayload, SkillsSnapshot, SummarizeQueuedPayload } from "./types";
import { isKeepAliveRunning, startKeepAlive, stopKeepAlive, installKeepAliveAutoResume } from "./keepAlive";
import { installSitterAutoRestore } from "./sitter";

function getIDFromHash(): string | null {
  const hash = window.location.hash; // #/uuid
  if (hash.startsWith("#/")) {
    return hash.slice(2);
  }
  return null;
}

export function useWS() {
  useEffect(() => {
    ws.connect();
    // One-shot per session: re-resume the AudioContext when the tab is
    // foregrounded after a long sleep. The actual start/stop is gated on
    // the server-side preference (see the `config` event handler below).
    installKeepAliveAutoResume();
    // Re-arm the sitter loop if the operator had it on before reload.
    // First tick happens after the saved interval — no risk of a flurry
    // of resume messages on every reload.
    installSitterAutoRestore();

    const onHashChange = () => {
      const id = getIDFromHash();
      const currentActive = $activeSessionID.get();
      if (id && id !== currentActive) {
        const sessionExists = $sessions.get().some((s) => s.ID === id);
        if (sessionExists) {
          setActiveSession(id);
          sendLoadMessages(id);
        }
      } else if (!id && currentActive) {
        setActiveSession(null);
      }
    };
    window.addEventListener("hashchange", onHashChange);

    const offs = [
      ws.on("_connected", () => {
        $connected.set(true);
        // Reconcile busy state against queued work instead of blindly
        // clearing it (task #726). The server appends an authoritative
        // agent_busy frame for every session to each sessions_list reply
        // (handleListSessions in internal/server/handlers_sessions.go), so
        // the sendListSessions() below re-marks sessions that are truly
        // still running. But a disconnect can swallow the final
        // agent_busy=false of a turn that ended while we were offline —
        // blindly clearing left such a session's queued messages with no
        // trigger that would ever flush them. Keep the busy flag only for
        // sessions that still have queued work awaiting that replay; when
        // the replay reports busy=false, the agent_busy handler below
        // flushes the queue as a fresh send_message.
        const queuedIDs = $messageQueue.get();
        const nextBusy = new Set<string>();
        for (const id of $busySessions.get()) {
          if (queuedIDs.has(id)) nextBusy.add(id);
        }
        $busySessions.set(nextBusy);
        sendListSessions();
        ws.send("get_config");
        ws.send("get_skills");
        // Sync theme from localStorage to server on every (re)connect
        // so the server's state always matches what the client has saved locally.
        const localTheme = localStorage.getItem("rush_theme");
        if (localTheme) {
          ws.send("set_theme", { theme: localTheme });
        }
      }),

      ws.on("_disconnected", () => {
        $connected.set(false);
        // Stop the BT keep-alive noise loop while the backend is
        // unreachable — no point holding the audio device awake for a
        // session that isn't running. It comes back automatically once
        // the backend reconnects and re-sends `config` (see the "config"
        // handler below), which re-applies the user's preference.
        if (isKeepAliveRunning()) stopKeepAlive();
      }),

      ws.on("session_created", (msg: WSMessage) => {
        const s = msg.payload as Session;
        console.log("[useWS] session_created:", s.ID, "ParentSessionID:", s.ParentSessionID);
        upsertSession(s);
        recordSessionLiveUpdate(s);
        if (s.ParentSessionID) {
          registerSubAgentSession(s.ID, s.ParentSessionID);
          sendLoadMessages(s.ID);
          return;
        }
        setActiveSession(s.ID);
        sendLoadMessages(s.ID);
      }),
      ws.on("session_updated", (msg: WSMessage) => {
        const session = msg.payload as Session;
        upsertSession(session);
        recordSessionLiveUpdate(session);
      }),
      ws.on("session_deleted", (msg: WSMessage) => {
        const id = (msg.payload as { ID: string }).ID;
        removeSession(id);
        recordSessionLiveDelete(id);
        if ($activeSessionID.get() === id) {
          setActiveSession(null);
        }
      }),
      ws.on("sessions_list", (msg: WSMessage) => {
        // Stale-reply guard (task #690): drop replies superseded by a newer
        // list_sessions — the newer reply reflects a more current DB read.
        if (isStaleSessionsListReply(msg.id)) return;
        const raw = (msg.payload as Session[]) ?? [];
        // Live-race reconcile (task #690): a still-latest reply can carry a
        // snapshot read BEFORE live pushes applied while the request was in
        // flight; replay those deltas on top instead of wholesale-replacing.
        // Untagged (back-compat) replies keep the plain replace and leave the
        // delta log intact so an in-flight tagged request stays protected.
        let sessions = raw;
        if (msg.id) {
          sessions = applySessionsSnapshot(raw, takeSessionsLiveDelta());
        } else {
          setSessions(raw);
        }

        // Offline-deletion reconcile (task #726): a session deleted while
        // this tab was disconnected never gets its authoritative
        // agent_busy replay (handleListSessions only replays sessions
        // that still exist), so both its queued messages and its busy
        // flag would be stranded forever — nothing would ever flush
        // them. Drop both for any id the fresh list no longer knows.
        // No-op on ordinary polls, where every queued/busy id is present.
        const liveIDs = new Set(sessions.map((s) => s.ID));
        for (const id of [...$messageQueue.get().keys(), ...$busySessions.get()]) {
          if (!liveIDs.has(id)) removeSessionQueueAndBusy(id);
        }

        // Foreign-owned active session: kick a load_messages refresh on
        // every sessions_list poll too. This guarantees we never sit
        // longer than the sessions poll interval (5s) without a fresh
        // history read, in case the dedicated 1.5s messages poll
        // missed a window during a pause.
        const activeID0 = $activeSessionID.get();
        if (activeID0) {
          const a = sessions.find((s) => s.ID === activeID0);
          if (a && a.OwnedExternal) {
            sendLoadMessages(activeID0);
          }
        }

        const topLevelSessions = sessions.filter((s) => !s.ParentSessionID);
        if (topLevelSessions.length === 0) {
          ws.send("create_session");
          return;
        }

        const hashID = getIDFromHash();
        const activeID = $activeSessionID.get();

        if (hashID) {
          const session = sessions.find((s) => s.ID === hashID);
          if (session) {
            if (activeID !== hashID) {
              setActiveSession(hashID);
              sendLoadMessages(hashID);
            }
            return;
          }
        }

        // If no valid hash or session not found, pick the most recent non-sub-agent session
        const latest = sessions.find((s) => !s.ParentSessionID);
        if (latest && activeID !== latest.ID) {
          setActiveSession(latest.ID);
          sendLoadMessages(latest.ID);
        }
      }),

      ws.on("message_created", (msg: WSMessage) => {
        const m = msg.payload as Message;
        if (isSubAgentSession(m.SessionID)) {
          upsertSubAgentMessage(m.SessionID, m);
          bumpLiveEventEpoch(m.SessionID);
          return;
        }
        const activeID = $activeSessionID.get();
        if (!activeID || m.SessionID !== activeID) return;
        upsertMessage(m);
        bumpLiveEventEpoch(m.SessionID);
        if (m.Role === "assistant" && m.Provider && m.Model) {
          trackModelUsage("smart", `${m.Provider}:::${m.Model}`);
        }
      }),
      ws.on("message_updated", (msg: WSMessage) => {
        const m = msg.payload as Message;
        if (isSubAgentSession(m.SessionID)) {
          upsertSubAgentMessage(m.SessionID, m);
          bumpLiveEventEpoch(m.SessionID);
          return;
        }
        const activeID = $activeSessionID.get();
        if (!activeID || m.SessionID !== activeID) return;
        if (m.Role === "assistant") {
          trackMessageParts(m.ID, m.Parts);
        }
        upsertMessage(m);
        bumpLiveEventEpoch(m.SessionID);
      }),
      ws.on("message_deleted", (msg: WSMessage) => {
        const m = msg.payload as Message;
        // Mirror the message_created/updated sub-agent routing: compaction
        // deletes sub-agent messages too, and the sub-agent block never
        // re-fetches once populated, so deletes must be applied in place.
        if (isSubAgentSession(m.SessionID)) {
          tombstoneMessage(m.SessionID, m.ID);
          removeSubAgentMessage(m.SessionID, m.ID);
          bumpLiveEventEpoch(m.SessionID);
          return;
        }
        // Only process messages for the active session
        const activeID = $activeSessionID.get();
        if (!activeID || m.SessionID !== activeID) return;
        tombstoneMessage(m.SessionID, m.ID);
        removeMessage(m.ID);
        bumpLiveEventEpoch(m.SessionID);
      }),
      ws.on("messages_list", (msg: WSMessage) => {
        // New envelope: { SessionID, Messages }. Old shape (raw array) is
        // kept as a fallback for back-compat with cached frontends, but the
        // backend now always wraps so we can route empty replies safely —
        // a lazy load_messages for an empty sub-agent session must NOT
        // overwrite the active main session's messages.
        const payload = msg.payload as
          | { SessionID?: string; Messages?: Message[] }
          | Message[]
          | undefined;
        let sid: string | undefined;
        let msgs: Message[] = [];
        if (Array.isArray(payload)) {
          msgs = payload;
          sid = msgs[0]?.SessionID;
        } else if (payload) {
          msgs = payload.Messages ?? [];
          sid = payload.SessionID ?? msgs[0]?.SessionID;
        }
        // Stale-reply guard (task #685): load_messages is fired from
        // several uncoordinated call sites (two independent OwnedExternal
        // pollers among them) and the server dispatches it through a
        // worker pool with no FIFO guarantee, so an older request's reply
        // can arrive after a newer one for the same session. Drop it —
        // the newer reply already reflects a more current DB read, and
        // applying the older one would regress the visible transcript.
        if (isStaleMessagesReply(sid, msg.id)) return;
        // Live-race guard (task #689): a still-latest reply can carry a
        // snapshot read BEFORE live pushes that were applied while the
        // request was in flight. When that happened, merge (ID-union +
        // delete tombstones) instead of wholesale-replacing; otherwise
        // keep the replace — it is what makes deletes/compaction converge.
        if (sid && isSubAgentSession(sid)) {
          applySubAgentMessagesSnapshot(sid, msgs, hasLiveEventsSinceRequest(sid));
          return;
        }
        // For the main chat: only apply if it's for the currently active
        // session (we might have polled in flight and the user switched).
        const activeID = $activeSessionID.get();
        if (sid && activeID && sid !== activeID) return;
        applyMessagesSnapshot(sid ?? "", msgs, hasLiveEventsSinceRequest(sid));
      }),

      ws.on("update_available", (msg: WSMessage) => {
        // The backend only sends this when a newer release exists, so the
        // payload never has to be interrogated for "is there an update".
        $updateAvailable.set(msg.payload as { current: string; latest: string });
      }),

      ws.on("config", (msg: WSMessage) => {
        const cfg = msg.payload as ConfigPayload;
        $config.set(cfg);
        // Apply theme from server (backend is source of truth)
        if (cfg.theme) {
          applyTheme(cfg.theme);
        }
        // Sync the WebAudio keep-alive runtime to the server preference.
        // Backend resolves nil → true, so we treat anything other than an
        // explicit false as "ON". AudioContext requires a user gesture;
        // if startKeepAlive runs before the user has clicked anything in
        // the page, the AudioContext is constructed in suspended state —
        // installKeepAliveAutoResume + the visibilitychange listener
        // handle the resume once a gesture lands.
        const wantOn = cfg.keepAliveEnabled !== false;
        if (wantOn && !isKeepAliveRunning()) startKeepAlive();
        else if (!wantOn && isKeepAliveRunning()) stopKeepAlive();
        // Restore recent models from server (persisted across restarts)
        if (cfg.recentSmartModels?.length) {
          const keys = cfg.recentSmartModels.map(m => `${m.Provider}:::${m.Model}`);
          $recentSmartModels.set(keys);
        } else {
          $recentSmartModels.set([]);
        }
        if (cfg.recentFastModels?.length) {
          const keys = cfg.recentFastModels.map(m => `${m.Provider}:::${m.Model}`);
          $recentFastModels.set(keys);
        } else {
          $recentFastModels.set([]);
        }
      }),

      ws.on("mcp_state", (msg: WSMessage) =>
        $mcpState.set(msg.payload as MCPState)
      ),

      ws.on("agent_busy", (msg: WSMessage) => {
        const p = msg.payload as AgentBusyPayload;
        setSessionBusy(p.SessionID, p.Busy);
        // When the agent finishes, send all queued messages as one combined
        // message. Every attachment from every queued message rides on that
        // single flushed send, in queue order — mirroring how the texts are
        // joined with blank lines.
        if (!p.Busy) {
          const flushed = dequeueAllMessages(p.SessionID);
          if (flushed) {
            const payload: Record<string, unknown> = {
              sessionID: p.SessionID,
              content: flushed.content,
            };
            if (flushed.attachments.length > 0) {
              payload.attachments = flushed.attachments;
            }
            // sendQueued, never a bare send (task #726): the browser can
            // dispatch this final agent_busy=false frame while the socket
            // has already left OPEN (readyState flips before the close
            // event fires), so send() can return false here even though
            // the event arrived. A bare send would drop the just-drained
            // queue with no recovery path; parking the frame in the
            // offline outbox delivers it on the next reconnect instead.
            // The outbox-full fallback (100 parked frames) puts the
            // drained content back into the local queue so the reconnect
            // reconcile in the _connected handler can retry it later.
            if (!ws.sendQueued("send_message", payload)) {
              enqueueMessage(p.SessionID, flushed.content, flushed.attachments);
            }
          }
        }
      }),

      ws.on("summarize_queued", (msg: WSMessage) => {
        const p = msg.payload as SummarizeQueuedPayload;
        setSummarizeQueued(p.SessionID, p.Queued);
      }),

      ws.on("skills", (msg: WSMessage) => {
        setSkills((msg.payload as SkillsSnapshot).skills ?? []);
      }),

      ws.on("error", (msg: WSMessage) => {
        $agentError.set((msg.error as string) || "Unknown error");
        setTimeout(() => $agentError.set(null), 8000);
      }),
    ];

    // Visibility-gated polling. When the tab is hidden we let the WS
    // pubsub do its thing without any extra requests. When the tab is
    // visible:
    //   - poll sessions_list every 5s — keeps the sidebar fresh (titles,
    //     ownership, message counts) even when another rush process
    //     drives a session on the same .rush/.
    //   - if the active session is externally owned (another process
    //     holds the lock — OwnedExternal: true), poll its messages_list
    //     every 1.5s so the conversation streams visibly without going
    //     through that other process's in-memory pubsub.
    // On visibilitychange we tear down both intervals together, then
    // rebuild them and do an immediate fire when the tab comes back.
    let listInterval: number | undefined;
    let messagesInterval: number | undefined;

    const SESSIONS_POLL_MS = 5000;
    const FOLLOW_MESSAGES_POLL_MS = 1500;

    const stopPolling = () => {
      if (listInterval !== undefined) { clearInterval(listInterval); listInterval = undefined; }
      if (messagesInterval !== undefined) { clearInterval(messagesInterval); messagesInterval = undefined; }
    };

    const pollMessagesIfFollowed = () => {
      const id = $activeSessionID.get();
      if (!id) return;
      const sess = $sessions.get().find((s) => s.ID === id);
      if (!sess || !sess.OwnedExternal) return;
      sendLoadMessages(id);
    };

    const startPolling = () => {
      stopPolling();
      // Immediate refresh on tab focus so the user doesn't sit through
      // a full interval before the first update lands.
      sendListSessions();
      pollMessagesIfFollowed();
      listInterval = window.setInterval(() => sendListSessions(), SESSIONS_POLL_MS);
      messagesInterval = window.setInterval(pollMessagesIfFollowed, FOLLOW_MESSAGES_POLL_MS);
    };

    const onVisibility = () => {
      if (document.visibilityState === "visible") startPolling();
      else stopPolling();
    };

    document.addEventListener("visibilitychange", onVisibility);
    if (document.visibilityState === "visible") startPolling();

    return () => {
      window.removeEventListener("hashchange", onHashChange);
      document.removeEventListener("visibilitychange", onVisibility);
      stopPolling();
      offs.forEach((off) => off());
      ws.disconnect();
    };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps
}
