import { atom } from "nanostores";
import type { BatchWorkCompletion, BatchWorkNode, BatchWorkRoot, BatchWorkSnapshot } from "./types";

export interface BatchWorkState { lastGood: BatchWorkSnapshot | null }
export const $batchWorkBySession = atom<Map<string, BatchWorkState>>(new Map());

export function pickBatchWork(map: Map<string, BatchWorkState>, sessionID: string | null): BatchWorkState | null {
  if (!sessionID) return null;
  return map.get(sessionID) ?? null;
}

export function applyBatchWorkSnapshot(payload: BatchWorkSnapshot) {
  const next = new Map($batchWorkBySession.get());
  const previous = next.get(payload.sessionID)?.lastGood ?? null;
  const lastGood = payload.complete || previous === null
    ? { ...payload, roots: payload.roots ?? [], completions: payload.completions ?? [] }
    : { ...payload, roots: mergeRoots(previous.roots, payload.roots ?? []), completions: mergeCompletions(previous.completions, payload.completions ?? []) };
  next.set(payload.sessionID, { lastGood });
  $batchWorkBySession.set(next);
}

function mergeCompletions(previous: BatchWorkCompletion[], incoming: BatchWorkCompletion[]): BatchWorkCompletion[] {
  const byKey = new Map(previous.map((item) => [`${item.rootID}\u0000${item.nodeID}`, item]));
  for (const item of incoming) byKey.set(`${item.rootID}\u0000${item.nodeID}`, item);
  return [...byKey.values()];
}
function mergeRoots(previous: BatchWorkRoot[], incoming: BatchWorkRoot[]): BatchWorkRoot[] {
  const out: BatchWorkRoot[] = [];
  const seen = new Set<string>();
  const updates = new Map(incoming.map((root) => [root.rootID, root]));
  for (const prior of previous) {
    const next = updates.get(prior.rootID);
    if (!next) { out.push(prior); continue; }
    seen.add(next.rootID);
    const nodes = [...next.nodes];
    for (const node of prior.nodes) if (!nodes.some((item) => item.nodeID === node.nodeID)) nodes.push(node);
    out.push({ ...next, nodes });
  }
  for (const root of incoming) if (!seen.has(root.rootID)) out.push(root);
  return out;
}

export interface BatchTreeNode { node: BatchWorkNode; children: BatchTreeNode[]; childBatches: BatchTreeRoot[] }
export interface BatchTreeRoot { root: BatchWorkRoot; nodes: BatchTreeNode[] }

export function buildBatchTree(roots: BatchWorkRoot[]): BatchTreeRoot[] {
  const sorted = [...roots].sort((a, b) => a.rootID.localeCompare(b.rootID));
  const rootsByOwner = new Map<string, BatchWorkRoot[]>();
  for (const root of sorted) rootsByOwner.set(root.ownerSessionID, [...(rootsByOwner.get(root.ownerSessionID) ?? []), root]);
  const attached = new Set<string>();

  function buildRoot(root: BatchWorkRoot, ancestry: Set<string>): BatchTreeRoot {
    const nodes = [...root.nodes].sort((a, b) => a.ordinal - b.ordinal || a.nodeID.localeCompare(b.nodeID));
    const byID = new Map(nodes.map((node) => [node.nodeID, node]));
    const childrenByParent = new Map<string, BatchWorkNode[]>();
    const top: BatchWorkNode[] = [];
    for (const node of nodes) {
      const chain = new Set([node.nodeID]);
      let parentID = node.parentID;
      let malformed = false;
      while (parentID) {
        if (chain.has(parentID)) { malformed = true; break; }
        chain.add(parentID);
        const parentNode = byID.get(parentID);
        if (!parentNode) { malformed = true; break; }
        parentID = parentNode.parentID;
      }
      const parent = byID.get(node.parentID);
      if (!node.parentID || !parent || malformed) top.push(node);
      else childrenByParent.set(parent.nodeID, [...(childrenByParent.get(parent.nodeID) ?? []), node]);
    }

    function buildNode(node: BatchWorkNode, branch: Set<string>): BatchTreeNode {
      if (branch.has(node.nodeID)) return { node, children: [], childBatches: [] };
      const path = new Set(branch).add(node.nodeID);
      const nestedRoots = node.childSessionID ? rootsByOwner.get(node.childSessionID) ?? [] : [];
      const nested = nestedRoots.filter((candidate) => !ancestry.has(candidate.rootID) && !attached.has(candidate.rootID));
      for (const candidate of nested) attached.add(candidate.rootID);
      return {
        node,
        children: (childrenByParent.get(node.nodeID) ?? []).map((child) => buildNode(child, path)),
        childBatches: nested.map((candidate) => buildRoot(candidate, new Set(ancestry).add(candidate.rootID))),
      };
    }
    return { root, nodes: top.map((node) => buildNode(node, new Set())) };
  }

  const tree: BatchTreeRoot[] = [];
  const rootsWithLinks = new Set<string>();
  for (const root of sorted) {
    for (const node of root.nodes) {
      if (node.childSessionID) {
        for (const linked of rootsByOwner.get(node.childSessionID) ?? []) rootsWithLinks.add(linked.rootID);
      }
    }
  }
  for (const root of sorted) {
    if (attached.has(root.rootID) || rootsWithLinks.has(root.rootID)) continue;
    attached.add(root.rootID);
    tree.push(buildRoot(root, new Set([root.rootID])));
  }
  for (const root of sorted) {
    if (attached.has(root.rootID)) continue;
    attached.add(root.rootID);
    tree.push(buildRoot(root, new Set([root.rootID])));
  }
  return tree;
}
