import type { Message } from "../types";
import { ASYNC_NOTICE_PATTERNS } from "./ASYNC_NOTICE_PATTERNS.ts";
import { messageText } from "./messageText.ts";

/**
 * Whether a user message is a server-written async job completion notice.
 *
 * @param message - the message
 */
export function isAsyncCompletionNotice(message: Message): boolean {
  if (message.Role !== "user") return false;
  const text = messageText(message);
  return ASYNC_NOTICE_PATTERNS.finished.test(text) || ASYNC_NOTICE_PATTERNS.stopped.test(text);
}
