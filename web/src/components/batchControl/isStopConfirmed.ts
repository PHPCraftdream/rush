import type { BatchWorkNode } from "../../types";

/** Node states that confirm a stop took effect. */
const TERMINAL_STATES = ["cancelled", "completed", "failed", "timed_out", "interrupted", "skipped"];

/**
 * Whether a node reached a terminal state.
 *
 * @param node - the node
 */
export function isStopConfirmed(node: Pick<BatchWorkNode, "state">): boolean {
  return TERMINAL_STATES.includes(node.state);
}
