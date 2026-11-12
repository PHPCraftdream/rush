import { Carburetor } from "@react-carburetor/core";
import type { BatchWorkCompletion, BatchWorkNode, BatchWorkRoot, BatchWorkSnapshot } from "../types";

/** The last complete-or-merged snapshot of one session's batch work. */
export interface BatchWorkState {
  lastGood: BatchWorkSnapshot | null;
}

/** Structured batch-node snapshots, keyed by session id (a plain record, converted to a Map at the read edge). */
export interface IBatchWorkState {
  bySession: Record<string, BatchWorkState>;
}

/**
 * Replaces the batch table in the draft.
 *
 * @param draft - state to change
 * @param next - the new table
 */
const writeTable = (draft: IBatchWorkState, next: Record<string, BatchWorkState>): void => {
  draft.bySession = next;
};

/**
 * The merge key of a completion: its root and node.
 *
 * @param item - the completion
 */
const completionKey = (item: BatchWorkCompletion): string => `${item.rootID}\u0000${item.nodeID}`;

/**
 * A completion paired with its merge key.
 *
 * @param item - the completion
 */
const completionEntry = (item: BatchWorkCompletion): [string, BatchWorkCompletion] => [completionKey(item), item];

/**
 * A root paired with its id.
 *
 * @param root - the root
 */
const rootEntry = (root: BatchWorkRoot): [string, BatchWorkRoot] => [root.rootID, root];

/**
 * Merges incoming completions over the previous ones, keyed by root and node.
 *
 * @param previous - completions already held
 * @param incoming - completions of the partial snapshot
 */
const mergeCompletions = (
  previous: readonly BatchWorkCompletion[],
  incoming: readonly BatchWorkCompletion[],
): BatchWorkCompletion[] => {
  const byKey = new Map(previous.map(completionEntry));
  for (const item of incoming) byKey.set(completionKey(item), item);
  return [...byKey.values()];
};

/**
 * Appends the prior nodes an update left out.
 *
 * @param nodes - nodes of the update, extended in place
 * @param prior - the prior nodes
 */
const keepMissingNodes = (nodes: BatchWorkNode[], prior: readonly BatchWorkNode[]): void => {
  for (const node of prior) if (!nodes.some((item) => item.nodeID === node.nodeID)) nodes.push(node);
};

/**
 * Merges incoming roots over the previous ones; prior nodes missing from an update are kept.
 *
 * @param previous - roots already held
 * @param incoming - roots of the partial snapshot
 */
const mergeRoots = (previous: readonly BatchWorkRoot[], incoming: readonly BatchWorkRoot[]): BatchWorkRoot[] => {
  const out: BatchWorkRoot[] = [];
  const seen = new Set<string>();
  const updates = new Map(incoming.map(rootEntry));
  for (const prior of previous) {
    const next = updates.get(prior.rootID);
    if (!next) {
      out.push(prior);
      continue;
    }
    seen.add(next.rootID);
    const nodes = [...next.nodes];
    keepMissingNodes(nodes, prior.nodes);
    out.push({ ...next, nodes });
  }
  for (const root of incoming) if (!seen.has(root.rootID)) out.push(root);
  return out;
};

/** Holds the batch-work snapshots pushed by the server. */
export class BatchWorkCarburetor extends Carburetor<IBatchWorkState> {
  /**
   * Applies a session_batch_work snapshot: a complete one replaces, a partial one merges over the last good.
   *
   * @param payload - the snapshot
   */
  public applySnapshot(payload: BatchWorkSnapshot): void {
    const previous = this.getData().bySession[payload.sessionID]?.lastGood ?? null;
    const lastGood =
      payload.complete || previous === null
        ? { ...payload, roots: payload.roots ?? [], completions: payload.completions ?? [] }
        : {
            ...payload,
            roots: mergeRoots(previous.roots, payload.roots ?? []),
            completions: mergeCompletions(previous.completions, payload.completions ?? []),
          };
    this.update(writeTable, { ...this.getData().bySession, [payload.sessionID]: { lastGood } });
  }
}
