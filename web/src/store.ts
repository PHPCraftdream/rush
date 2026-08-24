import { atom, computed } from "nanostores";
import type { Session, Message, ContentPart, ConfigPayload, MCPState, Todo, SkillInfo } from "./types";

// ── Connection state ─────────────────────────────────────────────────────────
export const $connected = atom(false);
export const $authed = atom(false);

// ── Data ─────────────────────────────────────────────────────────────────────
export const $skills = atom<SkillInfo[]>([]);
export const $sessions = atom<Session[]>([]);
export const $activeSessionID = atom<string | null>(null);
export const $messages = atom<Message[]>([]);
export const $config = atom<ConfigPayload | null>(null);
// Set once per server start, and only when a newer release exists — the
// backend sends nothing otherwise, so a non-null value always means
// "there is an update". Replaces the notification path that was lost when
// the TUI was removed.
export const $updateAvailable = atom<{ current: string; latest: string } | null>(null);
export const $mcpState = atom<MCPState | null>(null);
export const $busySessions = atom<Set<string>>(new Set());
// Sessions where the user has queued a compact/summarise request.
export const $summarizeQueued = atom<Set<string>>(new Set());

// ── Sub-agent state ─────────────────────────────────────────────────────────
// Maps sub-agent session ID → parent session ID
export const $subAgentSessions = atom<Map<string, string>>(new Map());
// Maps sub-agent session ID → messages
export const $subAgentMessages = atom<Map<string, Message[]>>(new Map());

// Block breaks for zebra pattern: indices where a new visual block starts (5s gap)
export const $messageBlockBreaks = atom<Map<string, Set<number>>>(new Map());

// Monotonic counter incremented whenever the operator clicks "Collapse all
// spoilers" in the toolbar. Every collapsible component (ToolActivityGroup,
// ThinkingPart, SummaryMessage, BackgroundJobNotice) subscribes to it and
// closes its local open-state on each tick. Components use the value via
// a useEffect that skips the initial render (so just mounting doesn't
// collapse anything already open by default).
export const $collapseAllNonce = atom<number>(0);

export function collapseAllSpoilers() {
  $collapseAllNonce.set($collapseAllNonce.get() + 1);
}

// ── Actions ──────────────────────────────────────────────────────────────────
export function setSkills(skills: SkillInfo[]) {
  $skills.set(skills);
}

const LAST_SKILL_KEY = "crush_last_skill";
export const $lastUsedSkill = atom<string>(localStorage.getItem(LAST_SKILL_KEY) ?? "");
export function setLastUsedSkill(name: string) {
  $lastUsedSkill.set(name);
  localStorage.setItem(LAST_SKILL_KEY, name);
}

export function setSessions(sessions: Session[]) {
  $sessions.set(sessions);
}

export interface SessionsLiveDelta {
  upserts: Session[];
  deletedIDs: string[];
}

/** Applies a sessions_list snapshot, replaying any live session pushes that
 * raced the request (task #690). With an empty delta this is a plain
 * wholesale replace — the convergent behavior the 5s poll depends on. Returns
 * the reconciled list so routing decisions downstream use it, not the raw
 * snapshot. */
export function applySessionsSnapshot(sessions: Session[], live: SessionsLiveDelta): Session[] {
  let next = [...sessions];
  for (const id of live.deletedIDs) {
    next = next.filter((s) => s.ID !== id);
  }
  for (const s of live.upserts) {
    const idx = next.findIndex((x) => x.ID === s.ID);
    if (idx === -1) {
      next = [s, ...next];
    } else {
      next = [...next];
      next[idx] = s;
    }
  }
  $sessions.set(next);
  return next;
}

// ── Model History ────────────────────────────────────────────────────────────
// Recent models are stored on the backend and synced via WebSocket config.
// Local state is initialized empty and populated when config arrives.
export const $recentSmartModels = atom<string[]>([]);
export const $recentFastModels = atom<string[]>([]);

export function trackModelUsage(role: "smart" | "fast", modelKey: string) {
  const store = role === "smart" ? $recentSmartModels : $recentFastModels;

  const current = store.get();
  const next = [modelKey, ...current.filter((k) => k !== modelKey)].slice(0, 5);

  store.set(next);

  // Sync to backend - parse modelKey into provider and model
  const idx = modelKey.indexOf(":::");
  if (idx !== -1) {
    ws.send("track_model_usage", {
      modelType: role,
      provider: modelKey.slice(0, idx),
      model: modelKey.slice(idx + 3),
    });
  }
}

export function removeRecentModel(role: "smart" | "fast", modelKey: string) {
  const store = role === "smart" ? $recentSmartModels : $recentFastModels;

  const next = store.get().filter((k) => k !== modelKey);
  store.set(next);

  // Persist removal to server
  const idx = modelKey.indexOf(":::");
  if (idx !== -1) {
    ws.send("remove_recent_model", {
      modelType: role,
      provider: modelKey.slice(0, idx),
      model: modelKey.slice(idx + 3),
    });
  }
}

export function upsertSession(s: Session) {
  const list = $sessions.get();
  const idx = list.findIndex((x) => x.ID === s.ID);
  if (idx === -1) {
    $sessions.set([s, ...list]);
  } else {
    const next = [...list];
    next[idx] = s;
    $sessions.set(next);
  }
}

export function removeSession(id: string) {
  $sessions.set($sessions.get().filter((s) => s.ID !== id));
  // Drop the ws-layer request bookkeeping with the row (task #698): the
  // stale-reply floor and live-event epochs only matter while replies for
  // this session can still arrive and be applied.
  forgetSessionRequestState(id);
}

export function setActiveSession(id: string | null) {
  $activeSessionID.set(id);
  $messages.set([]);
  $agentError.set(null);
  if (id) {
    window.location.hash = `#/${id}`;
  } else {
    window.location.hash = "";
  }
}

export function setMessages(msgs: Message[]) {
  $messages.set(msgs);
}

// totalThinking returns the concatenated thinking text across every
// `thinking` part in a message. Reasoning can span multiple parts (one
// per reasoning round in a multi-step turn), so we sum them all rather
// than reading only Parts[0].
function totalThinking(parts: ContentPart[]): string {
  return partsOfKind(parts, "thinking")
    .map((p) => p.Thinking)
    .join("");
}

// totalText returns the concatenated answer text across every `text`
// part. An assistant turn can carry multiple text parts (text between
// tool calls, a closing summary, …); sum them so the length comparison
// reflects everything the user was shown.
function totalText(parts: ContentPart[]): string {
  return partsOfKind(parts, "text")
    .map((p) => p.Text)
    .join("");
}

// partsOfKind returns the parts of a single accumulating kind, preserving
// their in-message order. Used for both `thinking` and `text`.
function partsOfKind<T extends ContentPart["type"]>(
  parts: ContentPart[],
  kind: T,
): Extract<ContentPart, { type: T }>[] {
  return parts.filter(
    (p): p is Extract<ContentPart, { type: T }> => p.type === kind,
  );
}

// mergePreserveContent guards already-shown assistant content — BOTH the
// reasoning (inside the thinking spoiler) AND the answer text (rendered
// outside it) — against a wholesale-replace that would shrink or erase
// either one. This guard applies ONLY when the incoming message is NOT
// terminally finished; terminal updates are taken verbatim.
//
// The backend streams an assistant message by APPENDING reasoning/text to
// one in-memory message and broadcasting it via `message_updated`. Most
// deltas only grow content, but occasionally a stale snapshot arrives
// (the backend ticker re-broadcasts a moment before the latest delta
// lands) whose thinking OR text is shorter than what the user was just
// shown. Replacing the message wholesale in that case makes the content
// the operator was watching vanish mid-turn — the answer text blinks out
// the instant the thinking spoiler updates again.
//
// Rule, per accumulating kind (applies only to non-terminal incoming):
//   - THINKING: if incoming total thinking is shorter than existing →
//     keep existing thinking part(s); else use incoming's.
//   - TEXT: if incoming total text is shorter than existing → keep
//     existing text part(s); else use incoming's.
//   - TOOL / FINISH / OTHER (tool_call, tool_result, finish, anything
//     else): ALWAYS take from incoming — these advance legitimately.
//
// Rebuilt render order is the natural assistant order: thinking → text →
// tools/finish/other (in their incoming order). If NEITHER kind
// regressed (the normal growth case), the incoming message is returned
// verbatim — the cheapest path, and it preserves any incoming ordering
// nuance.
export function mergePreserveContent(existing: Message, incoming: Message): Message {
  // A terminally finished row is by definition not a mid-stream snapshot:
  // the server refuses every part-level edit/delete on a row that is not
  // terminally finished (internal/server/handlers_messages.go
  // updateMessageAndVerify), so a terminal broadcast that SHRINKS thinking
  // or text can only be a deliberate operator edit/delete that already
  // committed server-side. Take it verbatim — running the length guard
  // here would resurrect deleted parts and revert shortened edits, and the
  // rebuilt [thinking…, text…, advancing…] order would desynchronise the
  // client's Parts array from the DB's, mis-addressing every later
  // partIndex. The guard below now only ever sees genuinely mid-stream
  // (non-terminal) snapshots, which is exactly the stale re-broadcast
  // case it was written for.
  if (isTerminallyFinished(incoming.Parts)) {
    return incoming;
  }

  const existingThinking = totalThinking(existing.Parts);
  const incomingThinking = totalThinking(incoming.Parts);
  const existingText = totalText(existing.Parts);
  const incomingText = totalText(incoming.Parts);

  const thinkingRegressed = incomingThinking.length < existingThinking.length;
  const textRegressed = incomingText.length < existingText.length;

  // Normal growth or no regression on either accumulating kind — take
  // the update verbatim.
  if (!thinkingRegressed && !textRegressed) {
    return incoming;
  }

  // One or both kinds regressed: rebuild. Pick the longer side per kind
  // (existing wins ties because it is what the user is currently seeing),
  // then append all non-accumulating parts from incoming.
  const thinkingParts = thinkingRegressed
    ? partsOfKind(existing.Parts, "thinking")
    : partsOfKind(incoming.Parts, "thinking");
  const textParts = textRegressed
    ? partsOfKind(existing.Parts, "text")
    : partsOfKind(incoming.Parts, "text");
  const advancingParts = incoming.Parts.filter(
    (p) => p.type !== "thinking" && p.type !== "text",
  );

  return { ...incoming, Parts: [...thinkingParts, ...textParts, ...advancingParts] };
}

export function upsertMessage(msg: Message) {
  const list = $messages.get();
  const idx = list.findIndex((m) => m.ID === msg.ID);
  if (idx === -1) {
    $messages.set([...list, msg]);
  } else {
    const prev = list[idx];
    const next = [...list];
    if (prev.Role === "assistant" && msg.Role === "assistant") {
      const merged = mergePreserveContent(prev, msg);
      if (merged !== msg) {
        console.warn("[upsertMessage] content regression blocked", {
          id: msg.ID,
          prevText: totalText(prev.Parts).length,
          incomingText: totalText(msg.Parts).length,
          prevThinking: totalThinking(prev.Parts).length,
          incomingThinking: totalThinking(msg.Parts).length,
        });
      }
      next[idx] = merged;
    } else {
      next[idx] = msg;
    }
    $messages.set(next);
  }
}

export function removeMessage(id: string) {
  $messages.set($messages.get().filter((m) => m.ID !== id));
}

export function registerSubAgentSession(subSessionID: string, parentSessionID: string) {
  const map = new Map($subAgentSessions.get());
  map.set(subSessionID, parentSessionID);
  $subAgentSessions.set(map);
}

export function isSubAgentSession(sessionID: string): boolean {
  return $subAgentSessions.get().has(sessionID);
}

export function getParentSessionID(subSessionID: string): string | undefined {
  return $subAgentSessions.get().get(subSessionID);
}

export function upsertSubAgentMessage(sessionID: string, msg: Message) {
  const map = new Map($subAgentMessages.get());
  const msgs = [...(map.get(sessionID) ?? [])];
  const idx = msgs.findIndex((m) => m.ID === msg.ID);
  if (idx === -1) {
    msgs.push(msg);
  } else {
    msgs[idx] = msg;
  }
  map.set(sessionID, msgs);
  $subAgentMessages.set(map);
}

export function setSubAgentMessages(sessionID: string, msgs: Message[]) {
  const map = new Map($subAgentMessages.get());
  map.set(sessionID, msgs);
  $subAgentMessages.set(map);
}

// removeSubAgentMessage drops one message from a sub-agent session's
// slice. The WS message_deleted handler routes compaction deletions here
// so a compacted sub-agent block stops rendering messages the backend
// already deleted (sub-agent slices are otherwise only replaced wholesale
// by a fresh messages_list reply).
export function removeSubAgentMessage(sessionID: string, messageID: string) {
  const map = new Map($subAgentMessages.get());
  const msgs = map.get(sessionID);
  if (!msgs) return;
  const next = msgs.filter((m) => m.ID !== messageID);
  if (next.length === msgs.length) return;
  map.set(sessionID, next);
  $subAgentMessages.set(map);
}

// ── messages_list snapshot application (task #689) ──────────────────────────
//
// A load_messages reply is a DB snapshot read at some instant between the
// request's send and its reply. Live pushes (message_created/_updated/
// _deleted) may have been applied locally inside that window. When they
// were (detected via ws.ts's per-session live-event epoch), replacing the
// list wholesale would erase them, so the snapshot is MERGED instead:
//
//   - messages deleted by a live push are tombstoned and filtered out of
//     every merge until an epoch-clean snapshot clears the tombstones;
//   - an ID present in both lists keeps the fresher row: assistant pairs
//     go through mergePreserveContent (streaming growth can only win),
//     other roles compare UpdatedAt;
//   - IDs only in the live list (created after the snapshot's read) are
//     appended after the snapshot rows — they are by definition newer.
//
// An epoch-clean reply (nothing raced it) still replaces wholesale: that
// path is what makes server-side deletes/compaction converge on the
// client.

// Per-session IDs deleted by a live message_deleted push. Keyed by
// session so the main chat and each sub-agent transcript tombstone
// independently. Same lifetime class as ws.ts's latestLoadMessagesID:
// never eagerly cleared per event; bounded by sessions seen this tab.
const deletedMessageIDs = new Map<string, Set<string>>();

/** Records that messageID was deleted from sessionID by a live push, so
 * later merged snapshots that still contain the row (their DB read
 * predated the delete) keep it filtered out. */
export function tombstoneMessage(sessionID: string, messageID: string) {
  const set = deletedMessageIDs.get(sessionID);
  if (set) set.add(messageID);
  else deletedMessageIDs.set(sessionID, new Set([messageID]));
}

// mergeMessageLists combines a still-latest-but-stale snapshot with the
// live state that raced it. Pure: reads the tombstone set, writes
// nothing.
function mergeMessageLists(sessionID: string, existing: Message[], snapshot: Message[]): Message[] {
  const tomb = deletedMessageIDs.get(sessionID);
  const snap = tomb ? snapshot.filter((m) => !tomb.has(m.ID)) : snapshot;
  const existingByID = new Map(existing.map((m) => [m.ID, m]));
  const merged = snap.map((sm) => {
    const live = existingByID.get(sm.ID);
    if (!live) return sm;
    if (live.Role === "assistant" && sm.Role === "assistant") {
      return mergePreserveContent(live, sm);
    }
    // Non-assistant overlap: take the row with the newer UpdatedAt; a
    // tie keeps the live row (what the operator is looking at).
    return sm.UpdatedAt > live.UpdatedAt ? sm : live;
  });
  const snapIDs = new Set(snap.map((m) => m.ID));
  const liveTail = existing.filter((m) => !snapIDs.has(m.ID));
  return [...merged, ...liveTail];
}

/** Applies a messages_list snapshot to the main chat. `liveRaced` says a
 * live push was applied since the request was sent (ws.ts epoch check). */
export function applyMessagesSnapshot(sessionID: string, msgs: Message[], liveRaced: boolean) {
  if (!liveRaced) {
    setMessages(msgs);
    // A clean read postdates any recorded delete, so it cannot contain
    // tombstoned rows — dropping the set bounds memory.
    deletedMessageIDs.delete(sessionID);
    return;
  }
  $messages.set(mergeMessageLists(sessionID, $messages.get(), msgs));
}

/** Sub-agent transcript counterpart of applyMessagesSnapshot. */
export function applySubAgentMessagesSnapshot(sessionID: string, msgs: Message[], liveRaced: boolean) {
  if (!liveRaced) {
    setSubAgentMessages(sessionID, msgs);
    deletedMessageIDs.delete(sessionID);
    return;
  }
  const map = new Map($subAgentMessages.get());
  map.set(sessionID, mergeMessageLists(sessionID, map.get(sessionID) ?? [], msgs));
  $subAgentMessages.set(map);
}

const _msgPartTracker = new Map<string, { time: number; count: number }>();

export function trackMessageParts(msgID: string, parts: { type: string }[]) {
  const count = parts.length;
  const now = Date.now();
  const prev = _msgPartTracker.get(msgID);

  if (prev && count > prev.count && now - prev.time > 5000) {
    const newPart = parts[prev.count];
    if (newPart && newPart.type !== "tool_result" && newPart.type !== "finish") {
      const map = new Map($messageBlockBreaks.get());
      const set = new Set(map.get(msgID) ?? []);
      set.add(prev.count);
      map.set(msgID, set);
      $messageBlockBreaks.set(map);
    }
  }

  _msgPartTracker.set(msgID, { time: now, count });
}

export function setSessionBusy(sessionID: string, busy: boolean) {
  const s = new Set($busySessions.get());
  if (busy) s.add(sessionID);
  else s.delete(sessionID);
  $busySessions.set(s);
}

// Per-session model overrides: removed in favor of global selection
// Now using the session object from DB as source of truth.

export function getDefaultModelKey(role: "smart" | "fast", config: ConfigPayload | null): string {
  const entry = config?.models?.[role];
  if (entry) return `${entry.Provider}:::${entry.Model}`;
  return "";
}

import { ws, forgetSessionRequestState } from "./ws";
import { logClientEvent } from "./telemetry";
import { isTerminallyFinished } from "./components/Message/textParts";

export function updateTodos(sessionID: string, todos: Todo[]) {
  // Optimistic local update
  const sessions = $sessions.get();
  const idx = sessions.findIndex((s) => s.ID === sessionID);
  if (idx !== -1) {
    const next = [...sessions];
    next[idx] = { ...next[idx], Todos: todos };
    $sessions.set(next);
  }
  logClientEvent("update_todos", { sessionID, count: todos.length, todos: todos.map((t) => ({ content: t.content, status: t.status })) });
  ws.send("update_todos", { sessionID, todos });
}

export function setSessionModels(sessionID: string, smartKey: string | null, fastKey: string | null) {
  const parse = (key: string | null) => {
    if (!key) return null;
    const idx = key.indexOf(":::");
    if (idx === -1) return null;
    return { provider: key.slice(0, idx), model: key.slice(idx + 3) };
  };

  const large = parse(smartKey);
  const small = parse(fastKey);

  // Optimistic local update so the UI reflects the change immediately
  const sessions = $sessions.get();
  const idx = sessions.findIndex((s) => s.ID === sessionID);
  if (idx !== -1) {
    const next = [...sessions];
    next[idx] = {
      ...next[idx],
      ...(large ? { SmartModelProvider: large.provider, SmartModelID: large.model } : {}),
      ...(small ? { FastModelProvider: small.provider, FastModelID: small.model } : {}),
    };
    $sessions.set(next);
  }

  ws.send("set_session_models", {
    sessionID,
    smartModel: large,
    fastModel: small,
  });
}

// clearSessionModelSlot removes the session's explicit override for ONE
// slot, falling back to inheriting the folder/system default (task #467).
//
// This is NOT the same as calling setSessionModels with an empty key: an
// empty/null key there means "don't touch this slot" (parse() maps both to
// JS `null`, which is omitted from the WS payload and read as "leave alone"
// server-side — see internal/server/handlers.go's handleSetSessionModels).
// Clearing instead requires sending a NON-null object with an explicit empty
// provider/model, which internal/session's UpdateModels reads as "wipe this
// override" — distinct from a nil argument ("don't touch"). Only ONE of the
// two commands is sent (the untouched slot key is omitted entirely), so the
// other slot's override is left exactly as it was.
export function clearSessionModelSlot(sessionID: string, modelType: "smart" | "fast" | "worker" | "reviewer") {
  const sessions = $sessions.get();
  const idx = sessions.findIndex((s) => s.ID === sessionID);
  const clearedFields: Record<string, string> = {
    smart: "SmartModel",
    fast: "FastModel",
    worker: "WorkerModel",
    reviewer: "ReviewerModel",
  };
  const prefix = clearedFields[modelType];
  if (idx !== -1) {
    const next = [...sessions];
    next[idx] = { ...next[idx], [`${prefix}Provider`]: "", [`${prefix}ID`]: "" };
    $sessions.set(next);
  }

  ws.send("set_session_models", {
    sessionID,
    [`${modelType}Model`]: { provider: "", model: "" },
  });
}

export function setSessionReasoningEffort(
  sessionID: string,
  smartEffort: string | null,
  fastEffort: string | null,
) {
  const sessions = $sessions.get();
  const idx = sessions.findIndex((s) => s.ID === sessionID);
  if (idx !== -1) {
    const next = [...sessions];
    next[idx] = {
      ...next[idx],
      ...(smartEffort ? { SmartModelReasoningEffort: smartEffort } : {}),
      ...(fastEffort ? { FastModelReasoningEffort: fastEffort } : {}),
    };
    $sessions.set(next);
  }

  // Get current models to send with reasoning effort update
  if (idx !== -1) {
    const session = sessions[idx];
    if (session) {
      ws.send("set_session_models", {
        sessionID,
        smartModel: {
          provider: session.SmartModelProvider,
          model: session.SmartModelID,
          reasoning_effort: smartEffort || undefined,
        },
        fastModel: {
          provider: session.FastModelProvider,
          model: session.FastModelID,
          reasoning_effort: fastEffort || undefined,
        },
      });
    }
  }
}

// ── Theme ─────────────────────────────────────────────────────────────────────
// Theme is stored on the backend. localStorage is only used as a cache for
// instant page load (before WS connects) to avoid white flash.

const STORAGE_KEY_THEME = "crush_theme";

export function applyTheme(theme: string) {
  if (theme === "dark") {
    document.documentElement.classList.add("dark");
  } else {
    document.documentElement.classList.remove("dark");
  }
  // Cache to localStorage for instant load on next page refresh
  try { localStorage.setItem(STORAGE_KEY_THEME, theme); } catch {}
}

// Apply cached theme immediately on module load (before WS connects)
// This prevents white flash while waiting for backend connection.
;(function () {
  try {
    const cached = localStorage.getItem(STORAGE_KEY_THEME);
    if (cached) applyTheme(cached);
  } catch {}
})();

export function setTheme(theme: "light" | "dark") {
  applyTheme(theme);
  // Send to backend as source of truth
  ws.send("set_theme", { theme });
}

// setKeepAliveEnabled toggles the WebAudio keep-alive preference. The
// backend persists it to the global crush.json under
// options.keep_alive_enabled and broadcasts a fresh `config` event;
// useWS.ts reacts to that broadcast and starts/stops the local audio.
export function setKeepAliveEnabled(enabled: boolean) {
  ws.send("set_keep_alive", { enabled });
}

export function setProviderKey(providerID: string, apiKey: string) {
  ws.send("set_provider_key", { providerID, apiKey });
}

export function removeProviderKey(providerID: string) {
  ws.send("remove_provider_key", { providerID });
}

export function deleteMessage(messageID: string) {
  ws.send("delete_message", { messageID });
}

export function deleteMessages(messageIDs: string[]) {
  ws.send("delete_messages", { messageIDs });
}

export function updateMessageContent(messageID: string, content: string) {
  ws.send("update_message_content", { messageID, content });
}

export function updateMessageThinking(messageID: string, thinking: string) {
  ws.send("update_message_thinking", { messageID, thinking });
}

export function summarizeSession(sessionID: string) {
  ws.send("summarize_session", { sessionID });
}

export function cancelQueuedSummarize(sessionID: string) {
  ws.send("cancel_queued_summarize", { sessionID });
}

export function setSummarizeQueued(sessionID: string, queued: boolean) {
  const s = new Set($summarizeQueued.get());
  if (queued) s.add(sessionID);
  else s.delete(sessionID);
  $summarizeQueued.set(s);
}

export function deleteMessagePart(messageID: string, partIndex: number) {
  ws.send("delete_message_part", { messageID, partIndex });
}

export function updateMessagePart(messageID: string, partIndex: number, content: string) {
  ws.send("update_message_part", { messageID, partIndex, content });
}

export function togglePinMessage(messageID: string, pinned: boolean) {
  ws.send("toggle_pin_message", { messageID, pinned });
}

export function rerunFromMessage(messageID: string) {
  const sessionID = $activeSessionID.get();
  if (!sessionID) return;
  logClientEvent("rerun_message", { sessionID, messageID });
  ws.send("rerun_message", { messageID });
}

// collectTurnContent gathers the agent's response to one user prompt:
// thinking + text from every assistant message that follows the given user
// message, stopping at the next user message (or end of conversation). Tool
// calls and tool results are excluded — the operator wants the agent's prose,
// not its action log.
//
// Accepts EITHER a user message ID (canonical) OR any assistant/tool message
// ID belonging to that turn — the function walks backwards to find the turn's
// user message either way. This lets a "Copy all" button live on the agent's
// final message just as well as on the user's prompt.
//
// Returns "" if the user message has no agent response yet.
export function collectTurnContent(anyMessageID: string): string {
  const msgs = $messages.get();
  let startIdx = msgs.findIndex((m) => m.ID === anyMessageID);
  if (startIdx === -1) return "";
  // If we were handed a non-user message, walk back to the turn's user message.
  if (msgs[startIdx].Role !== "user") {
    let walk = startIdx;
    while (walk > 0 && msgs[walk].Role !== "user") walk--;
    if (msgs[walk].Role !== "user") return "";
    startIdx = walk;
  }

  const chunks: string[] = [];
  for (let i = startIdx + 1; i < msgs.length; i++) {
    const m = msgs[i];
    if (m.Hidden) continue;
    if (m.Role === "user") break; // next user turn — stop
    if (m.Role !== "assistant") continue; // skip tool-role messages
    for (const p of m.Parts) {
      if (p.type === "thinking") {
        const t = (p as { type: "thinking"; Thinking: string }).Thinking;
        if (t && t.trim()) chunks.push(`<thinking>\n${t}\n</thinking>`);
      } else if (p.type === "text") {
        const t = (p as { type: "text"; Text: string }).Text;
        if (t && t.trim()) chunks.push(t);
      }
    }
  }
  return chunks.join("\n\n");
}

// Wire shape of one file attachment in a send_message / inject_message
// frame. Mirrors the backend's SendMessagePayload.Attachments entry.
export interface WireAttachment {
  fileName: string;
  mimeType: string;
  data: string; // base64
}

export function sendWithFastModel(
  sessionID: string,
  content: string,
  attachments?: WireAttachment[]
) {
  const config = $config.get();
  const sess = $sessions.get().find((s) => s.ID === sessionID);
  let fastModel: { provider: string; model: string } | undefined;
  if (sess && sess.FastModelID) {
    fastModel = { provider: sess.FastModelProvider, model: sess.FastModelID };
  } else if (config?.models?.fast) {
    fastModel = { provider: config.models.fast.Provider, model: config.models.fast.Model };
  }
  const payload: Record<string, unknown> = { sessionID, content };
  if (fastModel) {
    payload.smartModel = fastModel;
  }
  if (attachments && attachments.length > 0) {
    payload.attachments = attachments;
  }
  ws.send("send_message", payload);
}

// ── Batch selection ───────────────────────────────────────────────────────────
export const $selectedMessageIDs = atom<Set<string>>(new Set());

export function toggleMessageSelection(id: string) {
  const s = new Set($selectedMessageIDs.get());
  if (s.has(id)) s.delete(id); else s.add(id);
  $selectedMessageIDs.set(s);
}

export function clearSelection() {
  $selectedMessageIDs.set(new Set());
}

// Select (only add, never remove) all IDs in the given array
export function selectMessageIDs(ids: string[]) {
  const s = new Set($selectedMessageIDs.get());
  for (const id of ids) s.add(id);
  $selectedMessageIDs.set(s);
}

// Last agent error to display in chat
export const $agentError = atom<string | null>(null);

// ── Message queue (client-side) ────────────────────────────────────────────────
export interface QueuedMessage {
  id: string;
  content: string;
  // Attachments captured from the composer when this message was queued;
  // they ride on the single flushed send when the turn ends.
  attachments?: WireAttachment[];
}

export const $messageQueue = atom<Map<string, QueuedMessage[]>>(new Map());

let _queueIDCounter = 0;
function newQueueID() { return `q-${++_queueIDCounter}`; }

export function enqueueMessage(
  sessionID: string,
  content: string,
  attachments?: WireAttachment[]
) {
  const q = new Map($messageQueue.get());
  q.set(sessionID, [
    ...(q.get(sessionID) ?? []),
    { id: newQueueID(), content, attachments },
  ]);
  $messageQueue.set(q);
}

export interface FlushedQueue {
  content: string;
  attachments: WireAttachment[];
}

// Drains the session's queue. Texts join with blank lines; every attachment
// from every queued message is concatenated in queue order, so a multi-
// message flush carries all of them on the single send.
export function dequeueAllMessages(sessionID: string): FlushedQueue | undefined {
  const q = new Map($messageQueue.get());
  const msgs = q.get(sessionID) ?? [];
  if (!msgs.length) return undefined;
  q.delete(sessionID);
  $messageQueue.set(q);
  return {
    content: msgs.map((m) => m.content).join("\n\n"),
    attachments: msgs.flatMap((m) => m.attachments ?? []),
  };
}

export function removeQueuedMessage(sessionID: string, id: string) {
  const q = new Map($messageQueue.get());
  const msgs = (q.get(sessionID) ?? []).filter((m) => m.id !== id);
  if (!msgs.length) q.delete(sessionID); else q.set(sessionID, msgs);
  $messageQueue.set(q);
}

export function updateQueuedMessage(sessionID: string, id: string, content: string) {
  const q = new Map($messageQueue.get());
  const msgs = (q.get(sessionID) ?? []).map((m) => m.id === id ? { ...m, content } : m);
  q.set(sessionID, msgs);
  $messageQueue.set(q);
}

// ── Settings actions ───────────────────────────────────────────────────────────

export function addContextPath(path: string) {
  ws.send("add_context_path", { path });
}

export function removeContextPath(path: string) {
  ws.send("remove_context_path", { path });
}

export function addSkillsPath(path: string) {
  ws.send("add_skills_path", { path });
}

export function removeSkillsPath(path: string) {
  ws.send("remove_skills_path", { path });
}

export function initializeProject(msgID?: string) {
  ws.send("initialize_project", {}, msgID);
}

// Scope is "global" or "local" (workspace); omitted/undefined defaults to
// "global" server-side, matching every scope-aware CLI command.
export type ConfigScope = "global" | "local";

export function addCustomProvider(payload: {
  id: string; name?: string; type: string; baseUrl: string; apiKey?: string;
  models?: { id: string; name: string; contextWindow?: number; costPer1mIn?: number; costPer1mOut?: number }[];
  peakHours?: { start: string; end: string } | null;
  scope?: ConfigScope;
}, msgID?: string) {
  ws.send("add_custom_provider", payload, msgID);
}

export function removeCustomProvider(id: string, scope?: ConfigScope, msgID?: string) {
  ws.send("remove_custom_provider", { id, scope }, msgID);
}

export function updateCustomProvider(payload: {
  oldId: string; id: string; name?: string; type: string; baseUrl: string; apiKey?: string;
  models?: { id: string; name: string; contextWindow?: number; costPer1mIn?: number; costPer1mOut?: number }[];
  peakHours?: { start: string; end: string } | null;
  scope?: ConfigScope;
}, msgID?: string) {
  ws.send("update_custom_provider", payload, msgID);
}

// setProviderPeakHours sets/clears ONLY the peak_hours field on ANY
// provider (built-in/catwalk-known like "anthropic"/"zai", or custom) — a
// targeted single-field write, unlike addCustomProvider/updateCustomProvider
// which replace every field and are only safe on a custom provider the
// client fully owns. This is what lets the UI manage peak hours for a
// built-in provider without knowing/round-tripping its type, base URL, API
// key, or model list.
export function setProviderPeakHours(payload: {
  id: string;
  peakHours: { start: string; end: string } | null;
  scope?: ConfigScope;
}, msgID?: string) {
  ws.send("set_provider_peak_hours", payload, msgID);
}

// ── My prompts history ─────────────────────────────────────────────────────
//
// Derived from $messages (current active session). One entry per visible
// user message, newest first — feeds:
//   • shell-style recall in the chat input (ArrowUp/Down on caret edge),
//   • the history dropdown (clickable list + jump-to button).
//
// Hidden / IsSummary / non-user messages are excluded. Empty texts are
// dropped so the recall stack only holds prompts the user could actually
// re-send.

export interface MyPromptItem {
  id: string;
  text: string;
}

function partsToText(parts: Array<{ type: string; Text?: string }>): string {
  let out = "";
  for (const p of parts) {
    if (p.type === "text" && p.Text) out += p.Text;
  }
  return out;
}

export const $myPrompts = computed($messages, (msgs): MyPromptItem[] => {
  const out: MyPromptItem[] = [];
  for (const m of msgs) {
    if (m.Hidden) continue;
    if (m.IsSummaryMessage) continue;
    if (m.Role !== "user") continue;
    const text = partsToText(m.Parts as unknown as Array<{ type: string; Text?: string }>).trim();
    if (!text) continue;
    out.push({ id: m.ID, text });
  }
  // Newest first — matches "press ↑ to get the previous prompt".
  return out.reverse();
});

// jumpToMessage — scrolls the transcript to the given message ID and flashes
// it briefly. The Message component renders id={`msg-${ID}`} on its row and
// the .msg-flash CSS class drives a one-second highlight.
export function jumpToMessage(id: string) {
  const el = document.getElementById(`msg-${id}`);
  if (!el) return;
  el.scrollIntoView({ behavior: "smooth", block: "center" });
  el.classList.remove("msg-flash"); // restart animation if already there
  // Reflow so the class re-add re-triggers the keyframes.
  void (el as HTMLElement).offsetWidth;
  el.classList.add("msg-flash");
  window.setTimeout(() => el.classList.remove("msg-flash"), 1100);
}
