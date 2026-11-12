import assert from "node:assert/strict";
import { test } from "node:test";
import { buildBatchTree } from "./batch/buildBatchTree.ts";
import type { BatchTreeNode, BatchTreeRoot } from "./batch/buildBatchTree.ts";
import { pickBatchWork } from "./batch/pickBatchWork.ts";
import { BatchWorkCarburetor } from "./state/BatchWorkCarburetor.ts";
import type { BatchWorkState } from "./state/BatchWorkCarburetor.ts";
import type { BatchWorkNode, BatchWorkRoot, BatchWorkSnapshot } from "./types.ts";

/**
 * Builds a batch node.
 *
 * @param nodeID - node id
 * @param parentID - parent node id, or empty for a top-level node
 * @param ordinal - sort position
 * @param patch - fields to override
 */
const node = (
  nodeID: string,
  parentID: string,
  ordinal: number,
  patch: Partial<BatchWorkNode> = {},
): BatchWorkNode => ({
  nodeID,
  parentID,
  kind: "task",
  taskKind: nodeID,
  toolCallID: "",
  state: "running",
  cause: "",
  ordinal,
  depth: 0,
  childSessionID: "",
  host: "alive",
  outcomeCode: "",
  outcomeSummary: "",
  outcomeRef: "",
  noticeSelected: false,
  ...patch,
});

/**
 * Builds a batch root owned by session s1.
 *
 * @param rootID - root id
 * @param nodes - its nodes
 */
const root = (rootID: string, nodes: BatchWorkNode[]): BatchWorkRoot => ({
  rootID,
  ownerSessionID: "s1",
  toolCallID: "",
  kind: "batch",
  taskKind: "",
  state: "running",
  cause: "",
  startedAt: 1,
  host: "alive",
  complete: true,
  counts: {
    pending: 0,
    starting: 0,
    running: nodes.length,
    cancelling: 0,
    completed: 0,
    failed: 0,
    cancelled: 0,
    timed_out: 0,
    interrupted: 0,
    skipped: 0,
    active: nodes.length,
  },
  delivery: { selected: 0, pending: 0, delivered: 0 },
  nodes,
});

/**
 * Builds a snapshot of session s1.
 *
 * @param partial - fields to override
 */
const snap = (partial: Partial<BatchWorkSnapshot>): BatchWorkSnapshot => ({
  v: 1,
  sessionID: "s1",
  complete: true,
  roots: [],
  completions: [],
  ...partial,
});

/**
 * Root id of a tree root.
 *
 * @param entry - the tree root
 */
const treeRootID = (entry: BatchTreeRoot): string => entry.root.rootID;

/**
 * Node id of a tree node.
 *
 * @param item - the tree node
 */
const treeNodeID = (item: BatchTreeNode): string => item.node.nodeID;

/**
 * Whether a tree node has no children.
 *
 * @param item - the tree node
 */
const hasNoChildren = (item: BatchTreeNode): boolean => item.children.length === 0;

/**
 * The store table as a Map, the shape pickBatchWork reads.
 *
 * @param store - the batch store
 */
const tableOf = (store: BatchWorkCarburetor): Map<string, BatchWorkState> =>
  new Map(Object.entries(store.getData().bySession));

/**
 * A fresh, empty store.
 */
const fresh = (): BatchWorkCarburetor => new BatchWorkCarburetor({ bySession: {} });

/**
 * Root id of a root.
 *
 * @param r - the root
 */
const rootIDOf = (r: BatchWorkRoot): string => r.rootID;

/**
 * Node id of a node.
 *
 * @param n - the node
 */
const nodeIDOf = (n: BatchWorkNode): string => n.nodeID;

test("complete snapshots replace, incomplete snapshots preserve roots, nodes and completions", () => {
  const store = fresh();
  store.applySnapshot(
    snap({
      roots: [root("a", [node("a", "", 0), node("child", "a", 1)])],
      completions: [
        { rootID: "old", nodeID: "old", outcomeCode: "ok", outcomeRef: "", state: "completed", outcomeSummary: "done" },
      ],
    }),
  );
  store.applySnapshot(snap({ complete: false, roots: [root("b", [node("b", "", 0)])] }));
  const merged = pickBatchWork(tableOf(store), "s1")!.lastGood!;
  assert.deepEqual(merged.roots.map(rootIDOf), ["a", "b"]);
  assert.deepEqual(merged.roots[0].nodes.map(nodeIDOf), ["a", "child"]);
  assert.equal(merged.completions.length, 1);
  store.applySnapshot(snap({ roots: [root("c", [node("c", "", 0)])] }));
  assert.deepEqual(pickBatchWork(tableOf(store), "s1")!.lastGood!.roots.map(rootIDOf), ["c"]);
});

test("incomplete snapshot with SAME root id preserves prior nodes via mergeRoots", () => {
  const store = fresh();
  store.applySnapshot(snap({ roots: [root("r1", [node("r1", "", 0), node("nB", "r1", 1)])] }));
  store.applySnapshot(snap({ complete: false, roots: [root("r1", [node("r1", "", 0)])] }));
  const merged = pickBatchWork(tableOf(store), "s1")!.lastGood!;
  assert.deepEqual(merged.roots[0].nodes.map(nodeIDOf), ["r1", "nB"]);
});

test("builds ordinal-sorted nested hierarchy and attaches delegated root beneath childSessionID link", () => {
  const outer = root("outer", [
    node("root-node", "", 0, { childSessionID: "child-session" }),
    node("later", "root-node", 4),
    node("first", "root-node", 2),
  ]);
  const inner = {
    ...root("inner", [node("inner-root", "", 0), node("deep", "inner-root", 1)]),
    ownerSessionID: "child-session",
  };
  const forest = buildBatchTree([inner, outer]);
  assert.deepEqual(forest.map(treeRootID), ["outer"]);
  assert.deepEqual(forest[0].nodes[0].children.map(treeNodeID), ["first", "later"]);
  assert.deepEqual(forest[0].nodes[0].childBatches.map(treeRootID), ["inner"]);
  assert.equal(forest[0].nodes[0].childBatches[0].nodes[0].children[0].node.nodeID, "deep");
});

test("malformed parents and cycles are retained without recursive loops", () => {
  const batch = root("r", [node("orphan", "missing", 0), node("a", "b", 1), node("b", "a", 2), node("valid", "", 3)]);
  const tree = buildBatchTree([batch]);
  assert.deepEqual(tree[0].nodes.map(treeNodeID), ["orphan", "a", "b", "valid"]);
  assert.equal(tree[0].nodes.every(hasNoChildren), true);
  const cyclicRoots = buildBatchTree([root("x", [node("xnode", "", 0, { childSessionID: "s1" })])]);
  assert.deepEqual(cyclicRoots.map(treeRootID), ["x"]);
});

test("unknown session and absent snapshot are empty", () => {
  const store = fresh();
  assert.equal(pickBatchWork(tableOf(store), null), null);
  assert.equal(pickBatchWork(tableOf(store), "missing"), null);
});
