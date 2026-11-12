// The awaiting-answer badge data (#1158) for the live-work Agents tab:
// one delegation item's question, display-ready. Kept in a plain module so
// the node --test unit suite can cover it without a DOM.
import type { LiveWorkItem } from "./types";

export interface AwaitingAnswerInfo {
  question: string;
}

/**
 * The badge data for a delegation paused on a question.
 *
 * Null when the item is not awaiting an answer (or carries no question text).
 *
 * @param item - the live-work item
 */
export function awaitingAnswerInfo(item: LiveWorkItem): AwaitingAnswerInfo | null {
  if (!item.awaitingAnswer) return null;
  const question = (item.awaitingQuestion ?? "").trim();
  if (!question) return null;
  return { question };
}
