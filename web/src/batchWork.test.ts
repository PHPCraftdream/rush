import assert from "node:assert/strict";
import { test } from "node:test";
import { $batchWorkBySession, applyBatchWorkSnapshot, buildBatchTree, pickBatchWork } from "./batchWork.ts";
import type { BatchWorkNode, BatchWorkRoot, BatchWorkSnapshot } from "./types.ts";

function node(nodeID: string, parentID: string, ordinal: number, patch: Partial<BatchWorkNode> = {}): BatchWorkNode {
  return { nodeID, parentID, kind: "task", taskKind: nodeID, toolCallID: "", state: "running", cause: "", ordinal, depth: 0, childSessionID: "", host: "alive", outcomeCode: "", outcomeSummary: "", outcomeRef: "", noticeSelected: false, ...patch };
}
function root(rootID: string, nodes: BatchWorkNode[]): BatchWorkRoot {
  return { rootID, ownerSessionID: "s1", toolCallID: "", kind: "batch", taskKind: "", state: "running", cause: "", startedAt: 1, host: "alive", complete: true, counts: { pending: 0, starting: 0, running: nodes.length, cancelling: 0, completed: 0, failed: 0, cancelled: 0, timed_out: 0, interrupted: 0, skipped: 0, active: nodes.length }, delivery: { selected: 0, pending: 0, delivered: 0 }, nodes };
}
function snap(partial: Partial<BatchWorkSnapshot>): BatchWorkSnapshot { return { v: 1, sessionID: "s1", complete: true, roots: [], completions: [], ...partial }; }

test("complete snapshots replace, incomplete snapshots preserve roots, nodes and completions", () => {
  $batchWorkBySession.set(new Map());
  applyBatchWorkSnapshot(snap({ roots: [root("a", [node("a", "", 0), node("child", "a", 1)])], completions: [{ rootID: "old", nodeID: "old", outcomeCode: "ok", outcomeRef: "", state: "completed", outcomeSummary: "done" }] }));
  applyBatchWorkSnapshot(snap({ complete: false, roots: [root("b", [node("b", "", 0)])] }));
  const merged = pickBatchWork($batchWorkBySession.get(), "s1")!.lastGood!;
  assert.deepEqual(merged.roots.map((r) => r.rootID), ["a", "b"]);
  assert.deepEqual(merged.roots[0].nodes.map((n) => n.nodeID), ["a", "child"]);
  assert.equal(merged.completions.length, 1);
  applyBatchWorkSnapshot(snap({ roots: [root("c", [node("c", "", 0)])] }));
  assert.deepEqual(pickBatchWork($batchWorkBySession.get(), "s1")!.lastGood!.roots.map((r) => r.rootID), ["c"]);
});

test("incomplete snapshot with SAME root id preserves prior nodes via mergeRoots", () => {
  $batchWorkBySession.set(new Map());
  applyBatchWorkSnapshot(snap({ roots: [root("r1", [node("r1", "", 0), node("nB", "r1", 1)])] }));
  applyBatchWorkSnapshot(snap({ complete: false, roots: [root("r1", [node("r1", "", 0)])] }));
  const merged = pickBatchWork($batchWorkBySession.get(), "s1")!.lastGood!;
  assert.deepEqual(merged.roots[0].nodes.map((n) => n.nodeID), ["r1", "nB"]);
});

test("builds ordinal-sorted nested hierarchy and attaches delegated root beneath childSessionID link", () => {
  const outer = root("outer", [node("root-node", "", 0, { childSessionID: "child-session" }), node("later", "root-node", 4), node("first", "root-node", 2)]);
  const inner = { ...root("inner", [node("inner-root", "", 0), node("deep", "inner-root", 1)]), ownerSessionID: "child-session" };
  const forest = buildBatchTree([inner, outer]);
  assert.deepEqual(forest.map((entry) => entry.root.rootID), ["outer"]);
  assert.deepEqual(forest[0].nodes[0].children.map((item) => item.node.nodeID), ["first", "later"]);
  assert.deepEqual(forest[0].nodes[0].childBatches.map((batch) => batch.root.rootID), ["inner"]);
  assert.equal(forest[0].nodes[0].childBatches[0].nodes[0].children[0].node.nodeID, "deep");
});

test("malformed parents and cycles are retained without recursive loops", () => {
  const batch = root("r", [node("orphan", "missing", 0), node("a", "b", 1), node("b", "a", 2), node("valid", "", 3)]);
  const tree = buildBatchTree([batch]);
  assert.deepEqual(tree[0].nodes.map((item) => item.node.nodeID), ["orphan", "a", "b", "valid"]);
  assert.equal(tree[0].nodes.every((item) => item.children.length === 0), true);
  const cyclicRoots = buildBatchTree([root("x", [node("xnode", "", 0, { childSessionID: "s1" })])]);
  assert.deepEqual(cyclicRoots.map((item) => item.root.rootID), ["x"]);
});

test("unknown session and absent snapshot are empty", () => {
  assert.equal(pickBatchWork($batchWorkBySession.get(), null), null);
  assert.equal(pickBatchWork($batchWorkBySession.get(), "missing"), null);
});
