import { atom } from "nanostores";
import type {
  BatchWorkRoot,
  BatchWorkSnapshot,
  LiveWorkItem,
  SessionLiveWorkPayload,
} from "./types";

// Batch-work store slice (R15a, internal/server/handlers_batch.go): the
// structured batch_nodes tree for the active session, delivered as full
// `session_batch_work` snapshots. INERT in practice unless the server runs
// with RUSH_BATCH_RUNTIME on -- flag off, the event never arrives and this
// slice stays empty, so the merged live-work view is exactly the old flat
// rows.
//
// Never-disappears rule: the store mutates ONLY on a delivered snapshot. A
// failed server-side read never sends one at all (the server still replies
// with an incomplete snapshot rather than dropping the reply), and
// incomplete snapshots MERGE -- so a transient read failure can neither
// clear nor replace previously live roots. Wire types live in types.ts.

export interface BatchWorkState {
  lastGood: BatchWorkSnapshot | null;
}

// Keyed by sessionID. No snapshot yet reads as null via pickBatchWork.
export const $batchWorkBySession = atom<Map<string, BatchWorkState>>(new Map());

// pickBatchWork takes the MAP as an explicit argument rather than reading
// $batchWorkBySession.get() itself. Same React Compiler constraint as
// pickLiveWork in store_livework.ts: the compiler memoizes a component's
// derived values from recognized reactive inputs only, and a plain function
// reaching into an atom via .get() is invisible to that analysis. Callers
// MUST pass the value returned by `useStore($batchWorkBySession)` here.
export function pickBatchWork(
  map: Map<string, BatchWorkState>,
  sessionID: string | null,
): BatchWorkState | null {
  if (!sessionID) return null;
  return map.get(sessionID) ?? null;
}

/** Applies a session_batch_work snapshot. complete=true REPLACES the last
 * good snapshot; complete=false MERGES the carried roots into it by rootID
 * (never drops a previously live root). No snapshot is ever applied for a
 * failed read -- the server simply does not send one. */
export function applyBatchWorkSnapshot(payload: BatchWorkSnapshot) {
  const next = new Map($batchWorkBySession.get());
  const prev = next.get(payload.sessionID)?.lastGood ?? null;
  const lastGood: BatchWorkSnapshot =
    payload.complete || prev === null
      ? { ...payload, roots: payload.roots ?? [] }
      : { ...payload, roots: mergeRoots(prev.roots, payload.roots ?? []) };
  next.set(payload.sessionID, { lastGood });
  $batchWorkBySession.set(next);
}

/** Merges incoming incomplete roots into the previous roots by rootID:
 * incoming roots overwrite their own ID, previously live roots without an
 * incoming counterpart are kept. */
function mergeRoots(prev: BatchWorkRoot[], incoming: BatchWorkRoot[]): BatchWorkRoot[] {
  const byID = new Map(prev.map((r) => [r.rootID, r]));
  for (const r of incoming) byID.set(r.rootID, r);
  return [...byID.values()];
}

/** Merge selector for the live-work panel: the old flat rows (commands +
 * agents) MINUS any row whose toolCallID matches a snapshot node's
 * toolCallID (empty toolCallIDs never dedupe). Both maps are explicit
 * arguments -- same React Compiler rule as pickBatchWork above. Flag off
 * (no snapshot state for the session) returns the flat rows unchanged. */
export function pickMergedLiveWork(
  liveWorkMap: Map<string, SessionLiveWorkPayload>,
  batchMap: Map<string, BatchWorkState>,
  sessionID: string | null,
): { commands: LiveWorkItem[]; agents: LiveWorkItem[] } {
  const flat = liveWorkMap.get(sessionID ?? "") ?? { commands: [], agents: [] };
  const snap = pickBatchWork(batchMap, sessionID)?.lastGood;
  if (!snap) return { commands: flat.commands, agents: flat.agents };
  const covered = new Set<string>();
  for (const root of snap.roots) {
    for (const node of root.nodes) {
      if (node.toolCallID) covered.add(node.toolCallID);
    }
  }
  const keep = (rows: LiveWorkItem[]) =>
    rows.filter((r) => !r.toolCallID || !covered.has(r.toolCallID));
  return { commands: keep(flat.commands), agents: keep(flat.agents) };
}
