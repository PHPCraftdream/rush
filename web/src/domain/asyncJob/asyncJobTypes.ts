/**
 * The terminal-cause statuses of an async job (stage 5b, contract section 5.1).
 *
 * "timed_out"/"stopped"/"cancelled" arrive via FormatAsyncCompletion's
 * stop/timeout/cancel wordings, matched by the stopped pattern.
 */
export type AsyncJobStatus = "finished" | "failed" | "timed_out" | "stopped" | "cancelled";

/** One job's outcome: status plus the result text. */
export interface AsyncJobCompletion {
  status: AsyncJobStatus;
  content: string;
}

/** Completions indexed out of the transcript's prose. */
export interface AsyncJobCompletionIndex {
  byToolCallID: Map<string, AsyncJobCompletion>;
  attachedNoticeIDs: Set<string>;
  /**
   * R12-C: async job_id (and legacy shell_id) to tool call ID, read from tool_result metadata.
   * The structured batch completion index uses it to attach a completion keyed by rootID
   * (the batch runtime's job identity) back onto the tool call block, so a reused
   * tool_call_id stays distinct per root. Read-only plumbing.
   */
  toolCallIDByJobID: Map<string, string>;
}
