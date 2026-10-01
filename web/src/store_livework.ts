import { atom } from "nanostores";
import { ws } from "./ws";
import type { LiveWorkItem, SessionLiveWorkPayload } from "./types";

// Live-work panel store slice (task #1059), split out of store.ts to keep
// it under the 1000-line limit. Server-side emitter lands in #1058 -- this
// slice only holds the client shape, the snapshot request, and the
// store-driven "expand and jump to this tool call" signal.

export interface SessionLiveWork {
  commands: LiveWorkItem[];
  agents: LiveWorkItem[];
}

const EMPTY_LIVE_WORK: SessionLiveWork = { commands: [], agents: [] };

// Keyed by sessionID. Sessions with no snapshot yet (or never requested)
// read as EMPTY_LIVE_WORK via pickLiveWork, not absent/undefined.
export const $liveWorkBySession = atom<Map<string, SessionLiveWork>>(new Map());

// pickLiveWork takes the MAP as an explicit argument rather than reading
// $liveWorkBySession.get() itself. This is required, not stylistic: the
// project's React Compiler babel plugin (rsbuild.config.ts) memoizes a
// component's derived values based on its recognized reactive inputs
// (props/state/hook return values) only. A plain function that reaches
// into a nanostore atom via .get() outside of useStore's tracked return
// value is invisible to that analysis -- the compiler treated
// `pickLiveWork(sessionID)` (its previous, atom-reading-internally shape)
// as a pure function of `sessionID` alone and memoized the JSX consuming
// its result across unrelated atom updates, so a fresh session_live_work
// snapshot landed in the atom (confirmed via the atom's own contents) but
// never reached the rendered tab bar. Callers MUST pass the value returned
// by `useStore($liveWorkBySession)` here, never call `.get()` themselves.
export function pickLiveWork(map: Map<string, SessionLiveWork>, sessionID: string | null): SessionLiveWork {
  if (!sessionID) return EMPTY_LIVE_WORK;
  return map.get(sessionID) ?? EMPTY_LIVE_WORK;
}

/** Applies a full session_live_work snapshot (push or get_session_live_work
 * reply) -- always replaces, never merges, per the wire contract. */
export function applyLiveWorkSnapshot(payload: SessionLiveWorkPayload) {
  const next = new Map($liveWorkBySession.get());
  next.set(payload.sessionID, {
    commands: payload.commands ?? [],
    agents: payload.agents ?? [],
  });
  $liveWorkBySession.set(next);
}

// Every get_session_live_work request is tagged with this prefix so
// useWS.ts's generic "error" handler can recognize its reply and swallow it
// silently instead of surfacing a banner (e.g. against an older server
// without the handler, or on a transport hiccup) -- both must read as
// "treat as empty", not as a user-visible failure.
const LIVE_WORK_REQUEST_PREFIX = "livework-";

/** Requests a fresh live-work snapshot for sessionID. Fire-and-forget: the
 * reply (once #1058 adds a handler) arrives as an ordinary
 * EventSessionLiveWork push, keyed by its own SessionID field, so nothing
 * here needs to correlate by request id for the success path. */
export function sendGetSessionLiveWork(sessionID: string) {
  const id = LIVE_WORK_REQUEST_PREFIX + crypto.randomUUID();
  ws.send("get_session_live_work", { sessionID }, id);
}

/** True if id belongs to a get_session_live_work request -- see
 * LIVE_WORK_REQUEST_PREFIX above. */
export function isLiveWorkRequestID(id: string | undefined): boolean {
  return !!id && id.startsWith(LIVE_WORK_REQUEST_PREFIX);
}

// ── Jump-to-tool-call signal ─────────────────────────────────────────────────
//
// LiveWorkList dispatches requestExpandToolCall on item click.
// ToolActivityGroup/ActionRow/SubAgentBlock subscribe (via
// useExpandToolCallSignal for the row-level ones) and force their matching
// tool call open; the panel itself then scrolls the anchor
// (`data-tool-call-id`) into view. Shape mirrors $collapseAllNonce's
// nonce-signal, but keyed to one target id per request instead of a bare
// counter.
export interface ExpandToolCallRequest {
  toolCallID: string;
  nonce: number;
}
export const $expandToolCallRequest = atom<ExpandToolCallRequest | null>(null);

let expandNonce = 0;
export function requestExpandToolCall(toolCallID: string) {
  expandNonce += 1;
  $expandToolCallRequest.set({ toolCallID, nonce: expandNonce });
}

// ── Wake schedules slice (stage 5b) ──────────────────────────────────────────
//
// The Schedules tab's data: wake_schedules rows for the active session,
// delivered as full `session_wake_schedules` snapshots (pushed on change by
// the same coalescing worker as live work, or replied to
// get_session_wake_schedules / cancel_wake_schedule). Same
// pass-the-map-explicitly rule as pickLiveWork above: the React Compiler
// can't see through atom reads inside plain functions.

import type { SessionWakeSchedulesPayload, WakeScheduleItem } from "./types";

const EMPTY_SCHEDULES: WakeScheduleItem[] = [];

export const $wakeSchedulesBySession = atom<Map<string, WakeScheduleItem[]>>(new Map());

export function pickWakeSchedules(map: Map<string, WakeScheduleItem[]>, sessionID: string | null): WakeScheduleItem[] {
  if (!sessionID) return EMPTY_SCHEDULES;
  return map.get(sessionID) ?? EMPTY_SCHEDULES;
}

/** Applies a full session_wake_schedules snapshot -- always replaces, never
 * merges, per the wire contract. */
export function applyWakeSchedulesSnapshot(payload: SessionWakeSchedulesPayload) {
  const next = new Map($wakeSchedulesBySession.get());
  next.set(payload.sessionID, payload.schedules ?? []);
  $wakeSchedulesBySession.set(next);
}

const WAKE_SCHEDULES_GET_PREFIX = "wakesched-get-";
const WAKE_SCHEDULES_CANCEL_PREFIX = "wakesched-cancel-";

/** Requests a fresh wake-schedule snapshot for sessionID. Fire-and-forget:
 * the reply arrives as an ordinary session_wake_schedules push keyed by its
 * own SessionID field. */
export function sendGetSessionWakeSchedules(sessionID: string) {
  ws.send("get_session_wake_schedules", { sessionID }, WAKE_SCHEDULES_GET_PREFIX + crypto.randomUUID());
}

/** Cancels one of the session's own wake schedules. The server replies with
 * a fresh session_wake_schedules snapshot; an unknown/foreign scheduleID is
 * an explicit error reply (surfaced by the dialog's inline error). */
export function sendCancelWakeSchedule(sessionID: string, scheduleID: string) {
  ws.send("cancel_wake_schedule", { sessionID, scheduleID }, WAKE_SCHEDULES_CANCEL_PREFIX + crypto.randomUUID());
}

/** True if id belongs to a wake-schedules GET request -- see
 * WAKE_SCHEDULES_GET_PREFIX above. */
export function isWakeSchedulesGetRequestID(id: string | undefined): boolean {
  return !!id && id.startsWith(WAKE_SCHEDULES_GET_PREFIX);
}

/** Inline error from a rejected cancel (unknown/foreign scheduleID,
 * transport): shown inside the still-open ConfirmDialog via its error prop,
 * never the global banner -- the operator's attention is already on the
 * dialog. Cleared by WakeScheduleList when the dialog closes or reopens. */
export const $wakeScheduleCancelError = atom<string | null>(null);

/** True if id belongs to a wake-schedules CANCEL request -- see
 * WAKE_SCHEDULES_CANCEL_PREFIX above. */
export function isWakeSchedulesCancelRequestID(id: string | undefined): boolean {
  return !!id && id.startsWith(WAKE_SCHEDULES_CANCEL_PREFIX);
}
