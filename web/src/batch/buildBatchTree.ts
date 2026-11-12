import type { BatchWorkNode, BatchWorkRoot } from "../types";

/** A batch node with its in-root children and the nested batches its child session owns. */
export interface BatchTreeNode {
  node: BatchWorkNode;
  children: BatchTreeNode[];
  childBatches: BatchTreeRoot[];
}

/** A batch root with its top-level nodes. */
export interface BatchTreeRoot {
  root: BatchWorkRoot;
  nodes: BatchTreeNode[];
}

/** Working tables shared by one buildBatchTree run. */
interface IBuildContext {
  rootsByOwner: Map<string, BatchWorkRoot[]>;
  attached: Set<string>;
}

/**
 * A node paired with its id.
 *
 * @param node - the node
 */
const nodeEntry = (node: BatchWorkNode): [string, BatchWorkNode] => [node.nodeID, node];

/**
 * Orders nodes by ordinal, then id.
 *
 * @param a - first node
 * @param b - second node
 */
const byOrdinal = (a: BatchWorkNode, b: BatchWorkNode): number =>
  a.ordinal - b.ordinal || a.nodeID.localeCompare(b.nodeID);

/**
 * Orders roots by id.
 *
 * @param a - first root
 * @param b - second root
 */
const byRootID = (a: BatchWorkRoot, b: BatchWorkRoot): number => a.rootID.localeCompare(b.rootID);

/**
 * Whether following parent links from a node loops or dead-ends.
 *
 * @param node - node to check
 * @param byID - nodes of its root by id
 */
const isMalformed = (node: BatchWorkNode, byID: ReadonlyMap<string, BatchWorkNode>): boolean => {
  const chain = new Set([node.nodeID]);
  let parentID = node.parentID;
  while (parentID) {
    if (chain.has(parentID)) return true;
    chain.add(parentID);
    const parentNode = byID.get(parentID);
    if (!parentNode) return true;
    parentID = parentNode.parentID;
  }
  return false;
};

/**
 * Builds one node, attaching nested batches owned by its child session exactly once.
 *
 * @param ctx - shared tables
 * @param childrenByParent - in-root children by parent node id
 * @param ancestry - root ids on the way down, to cut cycles
 * @param node - node to build
 * @param branch - node ids on the way down, to cut cycles
 */
const buildNode = (
  ctx: IBuildContext,
  childrenByParent: ReadonlyMap<string, BatchWorkNode[]>,
  ancestry: ReadonlySet<string>,
  node: BatchWorkNode,
  branch: ReadonlySet<string>,
): BatchTreeNode => {
  if (branch.has(node.nodeID)) return { node, children: [], childBatches: [] };
  const path = new Set(branch).add(node.nodeID);
  const nestedRoots = node.childSessionID ? (ctx.rootsByOwner.get(node.childSessionID) ?? []) : [];
  const nested = nestedRoots.filter(
    (candidate) => !ancestry.has(candidate.rootID) && !ctx.attached.has(candidate.rootID),
  );
  for (const candidate of nested) ctx.attached.add(candidate.rootID);
  return {
    node,
    children: (childrenByParent.get(node.nodeID) ?? []).map((child) =>
      buildNode(ctx, childrenByParent, ancestry, child, path),
    ),
    childBatches: nested.map((candidate) => buildRoot(ctx, candidate, new Set(ancestry).add(candidate.rootID))),
  };
};

/**
 * Builds one root: orders nodes, groups children under parents, drops malformed links to the top.
 *
 * @param ctx - shared tables
 * @param root - root to build
 * @param ancestry - root ids on the way down, to cut cycles
 */
function buildRoot(ctx: IBuildContext, root: BatchWorkRoot, ancestry: ReadonlySet<string>): BatchTreeRoot {
  const nodes = [...root.nodes].sort(byOrdinal);
  const byID = new Map(nodes.map(nodeEntry));
  const childrenByParent = new Map<string, BatchWorkNode[]>();
  const top: BatchWorkNode[] = [];
  for (const node of nodes) {
    const parent = byID.get(node.parentID);
    if (!node.parentID || !parent || isMalformed(node, byID)) top.push(node);
    else childrenByParent.set(parent.nodeID, [...(childrenByParent.get(parent.nodeID) ?? []), node]);
  }
  return { root, nodes: top.map((node) => buildNode(ctx, childrenByParent, ancestry, node, new Set())) };
}

/**
 * Builds the tree of batch roots, nesting a root under the node whose child session owns it.
 *
 * @param roots - every root of a session's batch snapshot
 */
export function buildBatchTree(roots: readonly BatchWorkRoot[]): BatchTreeRoot[] {
  const sorted = [...roots].sort(byRootID);
  const rootsByOwner = new Map<string, BatchWorkRoot[]>();
  for (const root of sorted) {
    rootsByOwner.set(root.ownerSessionID, [...(rootsByOwner.get(root.ownerSessionID) ?? []), root]);
  }
  const ctx: IBuildContext = { rootsByOwner, attached: new Set() };

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
    if (ctx.attached.has(root.rootID) || rootsWithLinks.has(root.rootID)) continue;
    ctx.attached.add(root.rootID);
    tree.push(buildRoot(ctx, root, new Set([root.rootID])));
  }
  for (const root of sorted) {
    if (ctx.attached.has(root.rootID)) continue;
    ctx.attached.add(root.rootID);
    tree.push(buildRoot(ctx, root, new Set([root.rootID])));
  }
  return tree;
}
