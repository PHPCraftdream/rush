import type { Message } from "../types";

/**
 * THE web-side authorship filter: shared by the arrow-history recall and the history dropdown.
 *
 * HumanTyped is computed server-side from structured fields only; the injected
 * isNotice predicate (isAsyncCompletionNotice) stays as the content-pattern
 * fallback for notice rows persisted before the BackgroundJobNotice column existed.
 *
 * @param m - the message
 * @param isNotice - content-pattern notice predicate
 */
export function isComposerTypedMessage(m: Message, isNotice: (m: Message) => boolean): boolean {
  if (!m.HumanTyped) return false;
  if (isNotice(m)) return false;
  return true;
}
