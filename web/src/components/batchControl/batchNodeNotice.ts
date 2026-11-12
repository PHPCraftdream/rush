import type { BatchWorkNode } from "../../types";
import { isStopConfirmed } from "./isStopConfirmed.ts";

/**
 * The notice line shown beside a node, or "" when there is none.
 *
 * @param node - the node
 */
export function batchNodeNotice(node: Pick<BatchWorkNode, "noticeSelected" | "noticeState" | "state">): string {
  if (node.noticeSelected && node.noticeState === "pending") return "Delayed notice";
  if (!node.noticeSelected && isStopConfirmed(node)) return "Notice suppressed";
  return "";
}
