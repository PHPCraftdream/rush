// Folds a sub-agent transcript into the facts the block header needs.

import type { TReadonly } from "@react-carburetor/core";
import type { Message } from "../../types";

/** Facts read off a sub-agent transcript. */
export interface ISubTranscriptSummary {
  completedCallIDs: Set<string>;
  model: string;
  effort: string;
}

/**
 * Reads the completed tool calls and the starting model/effort off the transcript.
 *
 * ToolCall.Finished on the sub-agent's own tool calls means "arguments fully
 * typed", not "tool returned" (same backend semantics as the parent's flag), so
 * per-row running state must come from the sub-session's own tool_result parts
 * — they live on role=tool messages inside this same transcript.
 *
 * Model/effort are what the sub-agent actually started with: the FIRST
 * assistant message with a non-empty value (not the last, which may belong to a
 * later handoff/compaction step) — mirrors the intent of errorFinish, which
 * instead wants the LAST assistant message for its own purpose (final finish reason).
 *
 * @param messages - the sub-agent transcript
 */
export function summarizeSubTranscript(messages: readonly TReadonly<Message>[]): ISubTranscriptSummary {
  const ids = new Set<string>();
  let firstModel = "";
  let firstEffort = "";
  for (const m of messages) {
    for (const p of m.Parts ?? []) {
      if (p.type === "tool_result") ids.add(p.ToolCallID);
    }
    if (m.Role === "assistant") {
      if (!firstModel && m.Model) firstModel = m.Model;
      if (!firstEffort && m.ReasoningEffort) firstEffort = m.ReasoningEffort;
    }
  }
  return { completedCallIDs: ids, model: firstModel, effort: firstEffort };
}
