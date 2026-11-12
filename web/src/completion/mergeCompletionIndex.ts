import type { AsyncJobCompletionIndex } from "../asyncJob/asyncJobTypes";
import type { BatchWorkCompletion } from "../types";
import type { CompletionRecord, MergedCompletionIndex } from "./completionTypes";
import { structuredStatus } from "./structuredStatus.ts";

/**
 * Merges the prose index (from messages) with the structured completions of one session batch snapshot.
 *
 * Prose entries win only where no structured outcome exists for their tool
 * call: the structured record always replaces the prose one on a join hit, and
 * is never re-parsed from prose. Records are keyed by rootID in `byRootID`, so
 * two roots reusing one tool_call_id stay distinct even though the join can
 * only resolve one of them onto the shared tool call.
 *
 * Join: a structured completion names its rootID and (R12-C wire fix) its own
 * toolCallID. Prefer the wire identity; fall back to the prose indexer
 * job_id to toolCallID metadata map for records written before the wire carried
 * toolCallID. A root with neither surfaces only in `byRootID`, never by guessing.
 *
 * @param prose - completions indexed from message prose
 * @param completions - structured completions of the session batch snapshot
 */
export function mergeCompletionIndex(
  prose: AsyncJobCompletionIndex,
  completions: readonly BatchWorkCompletion[] | null | undefined,
): MergedCompletionIndex {
  const byToolCallID = new Map<string, CompletionRecord>();
  for (const [toolCallID, completion] of prose.byToolCallID) {
    byToolCallID.set(toolCallID, { ...completion, source: "prose" });
  }
  const byRootID = new Map<string, CompletionRecord>();
  for (const comp of completions ?? []) {
    if (!comp.rootID) continue;
    const record: CompletionRecord = {
      status: structuredStatus(comp.state ?? "", comp.outcomeCode ?? ""),
      content: comp.outcomeSummary ?? "",
      source: "structured",
      rootID: comp.rootID,
    };
    byRootID.set(comp.rootID, record);
    const toolCallID = comp.toolCallID ?? prose.toolCallIDByJobID.get(comp.rootID);
    if (toolCallID) byToolCallID.set(toolCallID, record);
  }
  return { byToolCallID, byRootID, attachedNoticeIDs: new Set(prose.attachedNoticeIDs) };
}
