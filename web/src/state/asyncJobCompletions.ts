import { computed } from "@react-carburetor/core";
import { indexAsyncJobCompletions } from "../asyncJob/indexAsyncJobCompletions";
import { mergeCompletionIndex } from "../completion/mergeCompletionIndex";
import type { Message } from "../types";
import { batchWorkCarburetor } from "./batchWork";
import { messagesCarburetor } from "./messages";
import { sessionsCarburetor } from "./sessions";

/**
 * Completion index of the active session (R12-C): structured batch completions merged over the prose fallback.
 *
 * The prose indexer stays read-only; see completion/mergeCompletionIndex for the
 * merge rules and the structured-vs-prose behaviour boundary. The cast hands the
 * read-only tracked transcript to indexers that only read it.
 */
export const asyncJobCompletions = computed((read) => {
  const { messages } = read(messagesCarburetor);
  const { bySession } = read(batchWorkCarburetor);
  const { activeSessionID } = read(sessionsCarburetor);
  return mergeCompletionIndex(
    indexAsyncJobCompletions(messages as readonly Message[]),
    bySession[activeSessionID ?? ""]?.lastGood?.completions ?? null,
  );
});
