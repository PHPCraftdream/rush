import { atom } from "nanostores";
import type {
  BatchWorkRoot,
  BatchWorkSnapshot,
  LiveWorkItem,
} from "./types";
import type { SessionLiveWork } from "./store_livework";

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
 * incoming counterpart are kept. Within a shared root, nodes merge by
 * nodeID -- only a complete read can remove a previously live node. */
function mergeRoots(prev: BatchWorkRoot[], incoming: BatchWorkRoot[]): BatchWorkRoot[] {
  const out: BatchWorkRoot[] = [];
  const seen = new Set<string>();
  const incomingByID = new Map(incoming.map((r) => [r.rootID, r]));
  for (const p of prev) {
    const inc = incomingByID.get(p.rootID);
    if (!inc) {
      // Root only in prev: keep as-is.
      out.push(p);
      continue;
    }
    seen.add(inc.rootID);
    const nodes = [...inc.nodes];
    for (const pn of p.nodes) {
      if (!inc.nodes.some((n) => n.nodeID === pn.nodeID)) nodes.push(pn);
    }
    out.push({ ...inc, nodes });
  }
  for (const r of incoming) {
    if (!seen.has(r.rootID)) out.push(r);
  }
  return out;
}

/** Merge selector for the live-work panel: the old flat rows (commands +
 * agents) with any row whose toolCallID matches a snapshot node's
 * toolCallID REPLACED by that node's structured entry, and structured-only
 * nodes (no covered flat row) APPENDED in root/node order -- nothing is
 * dropped. Empty toolCallIDs never dedupe or render as appended rows.
 * Both maps are explicit arguments -- same React Compiler rule as
 * pickBatchWork above. Flag off (no snapshot state for the session) returns
 * the flat rows unchanged. */
export function pickMergedLiveWork(
  liveWorkMap: Map<string, SessionLiveWork>,
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
  const commands = keep(flat.commands);
  const agents = keep(flat.agents);
  for (const root of snap.roots) {
    for (const node of root.nodes) {
      if (!node.toolCallID) continue;
      const item: LiveWorkItem = {
        toolCallID: node.toolCallID,
        toolName: node.kind === "task" ? node.taskKind || node.kind : node.kind,
        title: node.outcomeSummary || node.taskKind || node.kind,
        childSessionID: node.childSessionID || undefined,
        startedAt: root.startedAt,
        status: node.state,
        reason: node.cause || undefined,
        lastActivityAt: undefined,
      };
      (node.childSessionID ? agents : commands).push(item);
    }
  }
  return { commands, agents };
}
