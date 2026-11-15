import type { AsyncJobCompletion } from "../asyncJob/asyncJobTypes";

/** Where a completion record came from. */
export type CompletionSource = "structured" | "prose";

/** A completion with its provenance. */
export interface CompletionRecord extends AsyncJobCompletion {
  source: CompletionSource;
  /** Set on structured records: the batch root the outcome belongs to. */
  rootID?: string;
}

/** The structured and prose completions of one session, merged. */
export interface MergedCompletionIndex {
  /** Keyed by tool call ID: the shape consumers already read (ToolActivityGroup to ActionRow jobCompletions map). */
  byToolCallID: Map<string, CompletionRecord>;
  /** Keyed by batch root ID: every structured completion, including roots whose tool_call_id could not be joined. */
  byRootID: Map<string, CompletionRecord>;
  attachedNoticeIDs: Set<string>;
}

/** One entry of the composer prompt recall history. */
export interface MyPromptItem {
  id: string;
  text: string;
}
