import type { TReadonly } from "@react-carburetor/core";
import { ws, requestTracker, sendLoadMessages, sendListSessions } from "./ws";
import {
  applyWakeSchedulesSnapshot,
  flushQueuedMessagesOnIdle,
  isLiveWorkRequestID,
  isWakeSchedulesCancelRequestID,
  isWakeSchedulesGetRequestID,
  removeSession,
  removeSessionQueueAndBusy,
  removeSubAgentState,
  setActiveSession,
  sendGetSessionLiveWork,
  sendGetSessionWakeSchedules,
  tombstoneMessage,
  trackModelUsage,
  applyTheme,
} from "./store";
import type { ModelRole } from "./store";
import type {
  WSMessage,
  Session,
  Message,
  ConfigPayload,
  MCPState,
  AgentBusyPayload,
  SkillsSnapshot,
  SummarizeQueuedPayload,
  SessionLiveWorkPayload,
  SessionWakeSchedulesPayload,
  BatchWorkSnapshot,
} from "./types";
import { keepAliveRuntime } from "./keepAlive/keepAliveRuntime";
import { reloadReconnectTranscript } from "./reconnectTranscript";
import { sitterLoop } from "./sitter/sitterLoop";
import type { IUpdateInfo } from "./state/NoticesCarburetor";
import type { ISessionsState } from "./state/SessionsCarburetor";
import { batchWorkCarburetor } from "./state/batchWork";
import { busyCarburetor } from "./state/busy";
import { configCarburetor } from "./state/config";
import { connectionCarburetor } from "./state/connection";
import { liveWorkCarburetor } from "./state/liveWork";
import { mcpCarburetor } from "./state/mcp";
import { messagesCarburetor } from "./state/messages";
import { noticesCarburetor } from "./state/notices";
import { queueCarburetor } from "./state/queue";
import { sessionsCarburetor } from "./state/sessions";
import { skillsCarburetor } from "./state/skills";
import { wakeCancelCarburetor } from "./state/wakeCancel";

/** Sessions list refresh period while the tab is visible. */
const SESSIONS_POLL_MS = 5000;
/** Messages refresh period for an externally owned active session. */
const FOLLOW_MESSAGES_POLL_MS = 1500;

/** The four model slots, with the config field that lists each one recent models. */
const RECENT_FIELDS: ReadonlyArray<readonly [ModelRole, keyof ConfigPayload]> = [
  ["smart", "recentSmartModels"],
  ["fast", "recentFastModels"],
  ["worker", "recentWorkerModels"],
  ["reviewer", "recentReviewerModels"],
];

/**
 * A session id.
 *
 * @param s - the session
 */
const sessionIDOf = (s: Session): string => s.ID;

/**
 * Whether a session is a top-level one (not a sub-agent session).
 *
 * @param s - the session
 */
const isTopLevel = (s: Session): boolean => !s.ParentSessionID;

/**
 * Whether the session has queued messages.
 *
 * @param id - session id
 */
const hasQueuedMessages = (id: string): boolean => queueCarburetor.hasQueued(id);

/**
 * The provider:::model key of a recent-model entry.
 *
 * @param m - the entry
 */
const recentKeyOf = (m: { Provider: string; Model: string }): string => `${m.Provider}:::${m.Model}`;

/**
 * Sends a flushed queue payload through the offline-aware path.
 *
 * @param payload - the send_message payload
 */
const sendQueuedPayload = (payload: Record<string, unknown>): boolean => ws.sendQueued("send_message", payload);

/** Hides the agent error banner after its display time. */
const clearAgentError = (): void => {
  noticesCarburetor.clearAgentError();
};

/**
 * Registers one frame handler.
 *
 * @param entry - frame type and its handler
 * @returns remover for the handler
 */
const registerHandler = (entry: readonly [string, (msg: WSMessage) => void]): (() => void) => ws.on(entry[0], entry[1]);

/**
 * Calls a remover.
 *
 * @param off - the remover
 */
const callOff = (off: () => void): void => off();

/** The session id in the URL hash (#/uuid), or null. */
const getIDFromHash = (): string | null => {
  const hash = window.location.hash; // #/uuid
  if (hash.startsWith("#/")) {
    return hash.slice(2);
  }
  return null;
};

/**
 * Sends a reconnect housekeeping frame that must not vanish silently (task #727).
 *
 * _connected can run while the socket has already flipped out of OPEN, the same
 * race class task #726 documented for the agent_busy flush, and a bare send()
 * would drop the frame. These refreshes are idempotent, so sendQueued (#692) is
 * safe: a failed send parks the frame in the offline outbox for the next
 * reconnect flush. Only the outbox-full refusal (100 frames) truly loses it, and
 * that gets logged.
 *
 * @param type - frame type
 * @param payload - frame payload
 */
const sendReconnectHousekeeping = (type: string, payload?: unknown): void => {
  if (!ws.sendQueued(type, payload)) {
    console.warn(`[useWS] reconnect ${type} frame dropped (offline outbox full)`);
  }
};

/** Routes a hash change to the matching session, when it exists. */
const onHashChange = (): void => {
  const id = getIDFromHash();
  const currentActive = sessionsCarburetor.getData().activeSessionID;
  if (id && id !== currentActive) {
    const sessionExists = sessionsCarburetor.findSession(id) !== undefined;
    if (sessionExists) {
      setActiveSession(id);
      sendLoadMessages(id);
    }
  } else if (!id && currentActive) {
    setActiveSession(null);
  }
};

/**
 * Keeps busy flags only for sessions that still have queued work (task #726).
 *
 * The server appends an authoritative agent_busy frame for every session to
 * each sessions_list reply (handleListSessions in internal/server/handlers_sessions.go),
 * so the list refresh re-marks sessions that are truly still running. But a
 * disconnect can swallow the final agent_busy=false of a turn that ended while
 * offline; blindly clearing left such a session queued messages with no trigger
 * that would ever flush them. When the replay reports busy=false, the agent_busy
 * handler flushes the queue as a fresh send_message.
 */
const reconcileBusyAgainstQueue = (): void => {
  const nextBusy = busyCarburetor.getData().busySessions.filter(hasQueuedMessages);
  busyCarburetor.setBusySessions(new Set(nextBusy));
};

/** Reconnect: reconcile busy state, refresh everything the offline gap may have missed. */
const onConnected = (): void => {
  connectionCarburetor.setConnected(true);
  reconcileBusyAgainstQueue();
  // list_sessions deliberately stays a plain send: parking a poll frame in the
  // outbox would only deliver a stale-watermark request. Its result is checked
  // (task #727): a silent drop would leave the sidebar stale until the next 5s
  // poll or reconnect.
  if (!sendListSessions()) {
    console.warn("[useWS] reconnect list_sessions refresh was not sent");
  }
  sendReconnectHousekeeping("get_config");
  sendReconnectHousekeeping("get_skills");
  // Sync theme from localStorage to server on every (re)connect
  // so the server state always matches what the client has saved locally.
  const localTheme = localStorage.getItem("rush_theme");
  if (localTheme) {
    sendReconnectHousekeeping("set_theme", { theme: localTheme });
  }
  const activeID = sessionsCarburetor.getData().activeSessionID;
  reloadReconnectTranscript(activeID, sendLoadMessages);
  // Live-work snapshot (task #1059): a reconnect can have missed pushes for
  // whatever session is active, so re-request it fresh, mirroring the
  // OwnedExternal messages_list refresh.
  if (activeID) {
    sendGetSessionLiveWork(activeID);
    ws.send("get_session_batch_work", { sessionID: activeID });
  }
};

/**
 * Link dropped: flag it and stop the keep-alive noise.
 *
 * There is no point holding the audio device awake for a session that is not
 * running; it comes back once the backend reconnects and re-sends `config`
 * (see onConfig), which re-applies the user preference.
 */
const onDisconnected = (): void => {
  connectionCarburetor.setConnected(false);
  if (keepAliveRuntime.isRunning()) keepAliveRuntime.stop();
};

/**
 * A session was created: add it, then activate it unless it is a sub-agent session.
 *
 * @param msg - the session_created frame
 */
const onSessionCreated = (msg: WSMessage): void => {
  const s = msg.payload as Session;
  console.log("[useWS] session_created:", s.ID, "ParentSessionID:", s.ParentSessionID);
  sessionsCarburetor.upsert(s);
  requestTracker.recordSessionLiveUpdate(s);
  if (s.ParentSessionID) {
    messagesCarburetor.registerSubAgentSession(s.ID, s.ParentSessionID);
    sendLoadMessages(s.ID);
    return;
  }
  setActiveSession(s.ID);
  sendLoadMessages(s.ID);
};

/**
 * A session changed.
 *
 * @param msg - the session_updated frame
 */
const onSessionUpdated = (msg: WSMessage): void => {
  const session = msg.payload as Session;
  sessionsCarburetor.upsert(session);
  requestTracker.recordSessionLiveUpdate(session);
};

/**
 * A session was deleted.
 *
 * @param msg - the session_deleted frame
 */
const onSessionDeleted = (msg: WSMessage): void => {
  const id = (msg.payload as { ID: string }).ID;
  removeSession(id);
  requestTracker.recordSessionLiveDelete(id);
  if (sessionsCarburetor.getData().activeSessionID === id) {
    setActiveSession(null);
  }
};

/**
 * Drops queued messages, busy flags and sub-agent state of sessions the fresh list no longer knows.
 *
 * Offline-deletion reconcile (tasks #726/#727): a session deleted while this tab
 * was disconnected never gets its authoritative agent_busy replay nor the
 * cascade session_deleted pushes, so its queue, busy flag and sub-agent branch
 * would be stranded forever. Sessions.Delete removes sub-sessions with their
 * parent and ListSessions only returns top-level rows, so a parent absent from
 * the fresh list means its whole sub-agent branch is gone. No-op on ordinary
 * polls, where every queued/busy id and every parent is present.
 *
 * @param sessions - the fresh list
 */
const reapOfflineDeletions = (sessions: readonly Session[]): void => {
  const liveIDs = new Set(sessions.map(sessionIDOf));
  for (const id of [...queueCarburetor.queuedSessionIDs(), ...busyCarburetor.getData().busySessions]) {
    if (!liveIDs.has(id)) removeSessionQueueAndBusy(id);
  }
  for (const parent of messagesCarburetor.parentSessionIDs()) {
    if (!liveIDs.has(parent)) removeSubAgentState(parent);
  }
};

/**
 * Picks the session to show after a list refresh: the one in the hash, else the most recent top-level one.
 *
 * @param sessions - the fresh list
 */
const routeAfterSessionsList = (sessions: readonly Session[]): void => {
  const hashID = getIDFromHash();
  const activeID = sessionsCarburetor.getData().activeSessionID;

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
  const latest = sessions.find(isTopLevel);
  if (latest && activeID !== latest.ID) {
    setActiveSession(latest.ID);
    sendLoadMessages(latest.ID);
  }
};

/**
 * The sessions list arrived: guard against stale replies, reconcile, then route.
 *
 * @param msg - the sessions_list frame
 */
const onSessionsList = (msg: WSMessage): void => {
  // Stale-reply guard (task #690): drop replies superseded by a newer
  // list_sessions; the newer reply reflects a more current DB read.
  if (requestTracker.isStaleSessionsListReply(msg.id)) return;
  const raw = (msg.payload as Session[]) ?? [];
  // Live-race reconcile (task #690): a still-latest reply can carry a snapshot
  // read BEFORE live pushes applied while the request was in flight; replay
  // those deltas on top instead of wholesale-replacing. Untagged (back-compat)
  // replies keep the plain replace and leave the delta log intact so an
  // in-flight tagged request stays protected.
  let sessions = raw;
  if (msg.id) {
    sessions = sessionsCarburetor.applySnapshot(raw, requestTracker.takeSessionsLiveDelta());
  } else {
    sessionsCarburetor.setSessions(raw);
  }

  reapOfflineDeletions(sessions);

  // Foreign-owned active session: kick a load_messages refresh on every
  // sessions_list poll too. This guarantees we never sit longer than the
  // sessions poll interval (5s) without a fresh history read, in case the
  // dedicated 1.5s messages poll missed a window during a pause.
  const activeID0 = sessionsCarburetor.getData().activeSessionID;
  if (activeID0) {
    const a = sessions.find((s) => s.ID === activeID0);
    if (a && a.OwnedExternal) {
      sendLoadMessages(activeID0);
    }
  }

  const topLevelSessions = sessions.filter(isTopLevel);
  if (topLevelSessions.length === 0) {
    ws.send("create_session");
    return;
  }
  routeAfterSessionsList(sessions);
};

/**
 * A message was created.
 *
 * @param msg - the message_created frame
 */
const onMessageCreated = (msg: WSMessage): void => {
  const m = msg.payload as Message;
  if (messagesCarburetor.isSubAgentSession(m.SessionID)) {
    messagesCarburetor.upsertSubAgentMessage(m.SessionID, m);
    requestTracker.bumpLiveEventEpoch(m.SessionID);
    return;
  }
  const activeID = sessionsCarburetor.getData().activeSessionID;
  if (!activeID || m.SessionID !== activeID) return;
  messagesCarburetor.upsertMessage(m);
  requestTracker.bumpLiveEventEpoch(m.SessionID);
  if (m.Role === "assistant" && m.Provider && m.Model) {
    trackModelUsage("smart", `${m.Provider}:::${m.Model}`);
  }
};

/**
 * A message changed (streaming growth included).
 *
 * @param msg - the message_updated frame
 */
const onMessageUpdated = (msg: WSMessage): void => {
  const m = msg.payload as Message;
  if (messagesCarburetor.isSubAgentSession(m.SessionID)) {
    messagesCarburetor.upsertSubAgentMessage(m.SessionID, m);
    requestTracker.bumpLiveEventEpoch(m.SessionID);
    return;
  }
  const activeID = sessionsCarburetor.getData().activeSessionID;
  if (!activeID || m.SessionID !== activeID) return;
  if (m.Role === "assistant") {
    messagesCarburetor.trackMessageParts(m.ID, m.Parts);
  }
  messagesCarburetor.upsertMessage(m);
  requestTracker.bumpLiveEventEpoch(m.SessionID);
};

/**
 * A message was deleted.
 *
 * Mirrors the created/updated sub-agent routing: compaction deletes sub-agent
 * messages too, and the sub-agent block never re-fetches once populated, so
 * deletes must be applied in place.
 *
 * @param msg - the message_deleted frame
 */
const onMessageDeleted = (msg: WSMessage): void => {
  const m = msg.payload as Message;
  if (messagesCarburetor.isSubAgentSession(m.SessionID)) {
    tombstoneMessage(m.SessionID, m.ID, m.DeleteGeneration);
    messagesCarburetor.removeSubAgentMessage(m.SessionID, m.ID);
    requestTracker.bumpLiveEventEpoch(m.SessionID);
    return;
  }
  // Only process messages for the active session
  const activeID = sessionsCarburetor.getData().activeSessionID;
  if (!activeID || m.SessionID !== activeID) return;
  tombstoneMessage(m.SessionID, m.ID, m.DeleteGeneration);
  messagesCarburetor.removeMessage(m.ID);
  requestTracker.bumpLiveEventEpoch(m.SessionID);
};

/** The decoded body of a messages_list frame. */
interface IMessagesListBody {
  sid: string | undefined;
  msgs: Message[];
  watermark: number | undefined;
}

/**
 * Decodes a messages_list payload.
 *
 * New envelope: { SessionID, Messages, Watermark }. The old shape (raw array)
 * is kept as a fallback for back-compat with cached frontends, but the backend
 * now always wraps so empty replies can be routed safely: a lazy load_messages
 * for an empty sub-agent session must NOT overwrite the active main session
 * messages.
 *
 * @param payload - the frame payload
 */
const decodeMessagesList = (
  payload: { SessionID?: string; Messages?: Message[]; Watermark?: number } | Message[] | undefined,
): IMessagesListBody => {
  if (Array.isArray(payload)) {
    return { sid: payload[0]?.SessionID, msgs: payload, watermark: undefined };
  }
  if (payload) {
    const msgs = payload.Messages ?? [];
    return { sid: payload.SessionID ?? msgs[0]?.SessionID, msgs, watermark: payload.Watermark };
  }
  return { sid: undefined, msgs: [], watermark: undefined };
};

/**
 * A transcript snapshot arrived.
 *
 * @param msg - the messages_list frame
 */
const onMessagesList = (msg: WSMessage): void => {
  const { sid, msgs, watermark } = decodeMessagesList(
    msg.payload as { SessionID?: string; Messages?: Message[]; Watermark?: number } | Message[] | undefined,
  );
  // Stale-reply guard (task #685): load_messages is fired from several
  // uncoordinated call sites (two independent OwnedExternal pollers among them)
  // and the server dispatches it through a worker pool with no FIFO guarantee,
  // so an older request reply can arrive after a newer one for the same session.
  // Drop it: the newer reply already reflects a more current DB read.
  if (requestTracker.isStaleMessagesReply(sid, msg.id)) return;
  // Live-race guard (task #689): a still-latest reply can carry a snapshot read
  // BEFORE live pushes that were applied while the request was in flight. When
  // that happened, merge (ID-union + delete tombstones) instead of
  // wholesale-replacing; otherwise keep the replace, which is what makes
  // deletes/compaction converge. The watermark check (task #731) augments this:
  // even an epoch-"clean" reply must not be trusted if its own watermark is
  // provably older than a delete already applied for this session.
  const liveRaced =
    requestTracker.hasLiveEventsSinceRequest(sid) || requestTracker.isSnapshotStaleForDeletes(sid, watermark);
  if (sid && messagesCarburetor.isSubAgentSession(sid)) {
    messagesCarburetor.applySubAgentMessagesSnapshot(sid, msgs, liveRaced);
    return;
  }
  // For the main chat: only apply if it is for the currently active session
  // (we might have polled in flight and the user switched).
  const activeID = sessionsCarburetor.getData().activeSessionID;
  if (sid && activeID && sid !== activeID) return;
  messagesCarburetor.applyMessagesSnapshot(sid ?? "", msgs, liveRaced);
};

/**
 * A newer release exists. The backend only sends this then, so the payload never has to be interrogated.
 *
 * @param msg - the update_available frame
 */
const onUpdateAvailable = (msg: WSMessage): void => {
  noticesCarburetor.setUpdateAvailable(msg.payload as IUpdateInfo);
};

/**
 * Restores the recent-model lists from the server (persisted across restarts).
 *
 * @param cfg - the config payload
 */
const restoreRecentModels = (cfg: ConfigPayload): void => {
  for (const [role, field] of RECENT_FIELDS) {
    const list = cfg[field] as { Provider: string; Model: string }[] | undefined;
    configCarburetor.setRecent(role, list?.length ? list.map(recentKeyOf) : []);
  }
};

/**
 * The config changed: store it, apply the theme, sync keep-alive and recent models.
 *
 * Syncs the WebAudio keep-alive runtime to the server preference. The backend
 * resolves nil to true, so anything other than an explicit false counts as ON.
 * AudioContext requires a user gesture; if start() runs before the user has
 * clicked anything, the AudioContext is constructed suspended and the
 * auto-resume + visibilitychange listeners handle the resume once a gesture lands.
 *
 * @param msg - the config frame
 */
const onConfig = (msg: WSMessage): void => {
  const cfg = msg.payload as ConfigPayload;
  configCarburetor.setConfig(cfg);
  // Apply theme from server (backend is source of truth)
  if (cfg.theme) {
    applyTheme(cfg.theme);
  }
  const wantOn = cfg.keepAliveEnabled !== false;
  if (wantOn && !keepAliveRuntime.isRunning()) keepAliveRuntime.start();
  else if (!wantOn && keepAliveRuntime.isRunning()) keepAliveRuntime.stop();
  restoreRecentModels(cfg);
};

/**
 * MCP server health arrived.
 *
 * @param msg - the mcp_state frame
 */
const onMcpState = (msg: WSMessage): void => {
  mcpCarburetor.setMcp(msg.payload as MCPState);
};

/**
 * A session agent started or finished a turn.
 *
 * When the agent finishes, all queued messages are sent as one combined
 * message. Every attachment from every queued message rides on that single
 * flushed send, in queue order, mirroring how the texts are joined with blank lines.
 *
 * @param msg - the agent_busy frame
 */
const onAgentBusy = (msg: WSMessage): void => {
  const p = msg.payload as AgentBusyPayload;
  busyCarburetor.setBusy(p.SessionID, p.Busy);
  if (!p.Busy) {
    flushQueuedMessagesOnIdle(p.SessionID, sendQueuedPayload);
  }
};

/**
 * A summarize request was queued or dequeued.
 *
 * @param msg - the summarize_queued frame
 */
const onSummarizeQueued = (msg: WSMessage): void => {
  const p = msg.payload as SummarizeQueuedPayload;
  busyCarburetor.setSummarizeQueued(p.SessionID, p.Queued);
};

/**
 * The skills list arrived.
 *
 * @param msg - the skills frame
 */
const onSkills = (msg: WSMessage): void => {
  skillsCarburetor.setSkills((msg.payload as SkillsSnapshot).skills ?? []);
};

/**
 * Live-work panel snapshot (task #1059): full every time, whether pushed or replying to get_session_live_work.
 *
 * Keyed by its own sessionID field, so no request correlation is needed.
 *
 * @param msg - the session_live_work frame
 */
const onLiveWork = (msg: WSMessage): void => {
  liveWorkCarburetor.applySnapshot(msg.payload as SessionLiveWorkPayload);
};

/**
 * Wake-schedules panel snapshot (stage 5b): full every time, whether pushed or replying to a get/cancel.
 *
 * @param msg - the session_wake_schedules frame
 */
const onWakeSchedules = (msg: WSMessage): void => {
  applyWakeSchedulesSnapshot(msg.payload as SessionWakeSchedulesPayload, msg.id);
};

/**
 * Batch work (R15a): structured batch-node snapshots.
 *
 * Unconditional since the H4 cutover: batch is the only runtime, so the event
 * always arrives for live sessions.
 *
 * @param msg - the session_batch_work frame
 */
const onBatchWork = (msg: WSMessage): void => {
  batchWorkCarburetor.applySnapshot(msg.payload as BatchWorkSnapshot);
};

/**
 * An error frame arrived.
 *
 * get_session_live_work has no server handler until #1058 lands: its "unknown
 * command" reply must be swallowed silently, not shown as a user-facing
 * failure. Wake-schedules GET requests swallow the same way. A cancel error is
 * explicit, but it lands INLINE in the still-open ConfirmDialog (its error
 * prop), not in the global banner the operator is not looking at.
 *
 * @param msg - the error frame
 */
const onError = (msg: WSMessage): void => {
  if (isLiveWorkRequestID(msg.id) || isWakeSchedulesGetRequestID(msg.id)) return;
  if (isWakeSchedulesCancelRequestID(msg.id)) {
    wakeCancelCarburetor.setError((msg.error as string) || "Failed to cancel the schedule");
    return;
  }
  noticesCarburetor.setAgentError((msg.error as string) || "Unknown error");
  setTimeout(clearAgentError, 8000);
};

/**
 * Picks the active session id out of the sessions state.
 *
 * @param state - the sessions state
 */
const selectActiveSessionID = (state: TReadonly<ISessionsState>): string | null => state.activeSessionID;

/**
 * Live-work snapshot on activation (task #1059).
 *
 * The active id is still null when connectWS runs, so the watch (change-only, no
 * immediate call) catches every activation from here on; setActiveSession is
 * the single choke point every activation path funnels through.
 *
 * @param id - the newly active session, or null
 */
const onActiveSessionChanged = (id: string | null): void => {
  if (id) {
    sendGetSessionLiveWork(id);
    sendGetSessionWakeSchedules(id);
    ws.send("get_session_batch_work", { sessionID: id });
  }
};

/** The frame handlers connectWS registers, by frame type. */
const FRAME_HANDLERS: ReadonlyArray<readonly [string, (msg: WSMessage) => void]> = [
  ["_connected", onConnected],
  ["_disconnected", onDisconnected],
  ["session_created", onSessionCreated],
  ["session_updated", onSessionUpdated],
  ["session_deleted", onSessionDeleted],
  ["sessions_list", onSessionsList],
  ["message_created", onMessageCreated],
  ["message_updated", onMessageUpdated],
  ["message_deleted", onMessageDeleted],
  ["messages_list", onMessagesList],
  ["update_available", onUpdateAvailable],
  ["config", onConfig],
  ["mcp_state", onMcpState],
  ["agent_busy", onAgentBusy],
  ["summarize_queued", onSummarizeQueued],
  ["skills", onSkills],
  ["session_live_work", onLiveWork],
  ["session_wake_schedules", onWakeSchedules],
  ["session_batch_work", onBatchWork],
  ["error", onError],
];

/**
 * Visibility-gated polling.
 *
 * When the tab is hidden the WS pubsub does its thing without any extra
 * requests. When visible: poll sessions_list every 5s, which keeps the sidebar
 * fresh (titles, ownership, message counts) even when another rush process
 * drives a session on the same .rush/; and if the active session is externally
 * owned (another process holds the lock, OwnedExternal: true), poll its
 * messages_list every 1.5s so the conversation streams visibly without going
 * through that other process in-memory pubsub. On visibilitychange both
 * intervals are torn down together, then rebuilt with an immediate fire when
 * the tab comes back.
 */
class VisibilityPoller {
  private listInterval: number | undefined;
  private messagesInterval: number | undefined;

  /** Clears both intervals. */
  private stopPolling(): void {
    if (this.listInterval !== undefined) {
      clearInterval(this.listInterval);
      this.listInterval = undefined;
    }
    if (this.messagesInterval !== undefined) {
      clearInterval(this.messagesInterval);
      this.messagesInterval = undefined;
    }
  }

  /** Reloads the active transcript when another process owns the session. */
  private pollMessagesIfFollowed(): void {
    const id = sessionsCarburetor.getData().activeSessionID;
    if (!id) return;
    const sess = sessionsCarburetor.findSession(id);
    if (!sess || !sess.OwnedExternal) return;
    sendLoadMessages(id);
  }

  /** Restarts both intervals with an immediate refresh, so the user does not wait out a full interval. */
  private startPolling(): void {
    this.stopPolling();
    sendListSessions();
    this.pollMessagesIfFollowed();
    this.listInterval = window.setInterval(sendListSessions, SESSIONS_POLL_MS);
    this.messagesInterval = window.setInterval(() => this.pollMessagesIfFollowed(), FOLLOW_MESSAGES_POLL_MS);
  }

  /** Starts or stops polling to match the tab visibility. */
  private onVisibility = (): void => {
    if (document.visibilityState === "visible") this.startPolling();
    else this.stopPolling();
  };

  /** Begins following the tab visibility. */
  public attach(): void {
    document.addEventListener("visibilitychange", this.onVisibility);
    if (document.visibilityState === "visible") this.startPolling();
  }

  /** Stops following the tab visibility and clears the intervals. */
  public detach(): void {
    document.removeEventListener("visibilitychange", this.onVisibility);
    this.stopPolling();
  }
}

/**
 * Opens the socket and routes every server event into the stores. Runs once
 * per mounted WsConnector (see components/WsConnector.tsx).
 *
 * @returns teardown that removes every listener/timer and closes the socket
 */
export function connectWS(): () => void {
  ws.connect();
  // One-shot per session: re-resume the AudioContext when the tab is
  // foregrounded after a long sleep. The actual start/stop is gated on the
  // server-side preference (see onConfig).
  const cleanupKeepAliveAutoResume = keepAliveRuntime.installAutoResume();
  // Re-arm the sitter loop if the operator had it on before reload. First tick
  // happens after the saved interval: no risk of a flurry of resume messages
  // on every reload.
  sitterLoop.restore();

  window.addEventListener("hashchange", onHashChange);

  const offs = [
    ...FRAME_HANDLERS.map(registerHandler),
    sessionsCarburetor.watch(selectActiveSessionID, onActiveSessionChanged),
  ];

  const poller = new VisibilityPoller();
  poller.attach();

  return () => {
    window.removeEventListener("hashchange", onHashChange);
    poller.detach();
    cleanupKeepAliveAutoResume();
    offs.forEach(callOff);
    ws.disconnect();
  };
}
