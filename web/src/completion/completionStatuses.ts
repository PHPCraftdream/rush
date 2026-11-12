import type { AsyncJobStatus } from "../asyncJob/asyncJobTypes";
import type { MergedCompletionIndex } from "./completionTypes";

/**
 * Flattens the merged index to the per-tool-call status map SubAgentBlock consumes.
 *
 * @param index - the merged index
 */
export function completionStatuses(index: MergedCompletionIndex): Map<string, AsyncJobStatus> {
  const statuses = new Map<string, AsyncJobStatus>();
  for (const [toolCallID, completion] of index.byToolCallID) {
    statuses.set(toolCallID, completion.status);
  }
  return statuses;
}
