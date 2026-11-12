import type { BatchWorkNode } from "../../types";

/**
 * The status label of a batch node.
 *
 * @param node - the node
 */
export function batchNodeStatus(node: Pick<BatchWorkNode, "state">): string {
  return node.state;
}
