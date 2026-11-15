import type { BatchWorkNode } from "../../domain/types";

/**
 * The status label of a batch node.
 *
 * @param node - the node
 */
export function batchNodeStatus(node: Pick<BatchWorkNode, "state">): string {
  return node.state;
}
