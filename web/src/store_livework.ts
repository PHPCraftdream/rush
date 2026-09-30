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
