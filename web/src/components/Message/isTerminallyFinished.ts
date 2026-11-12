// Mirror of the server's terminal-finish check.

import type { ContentPart } from "../../types";

/**
 * Whether a part is a finish part that is not partial.
 *
 * @param part - the message part
 */
const isTerminalFinish = (part: ContentPart): boolean => part.type === "finish" && !part.Partial;

/**
 * Mirrors message.Message.IsFinished() server-side (internal/message/content.go).
 *
 * True only when Parts contains a Finish part whose Partial flag is false/absent.
 * A message with no Finish part at all, or only a Partial one (written by the
 * server's auto-checkpoint ticker mid-stream), is NOT terminally finished — it
 * is still owned by an in-flight agent turn, which will overwrite any edit made
 * to it via its next checkpoint or, unconditionally, its terminal write (task #590).
 *
 * @param parts - the message parts
 */
export function isTerminallyFinished(parts: readonly ContentPart[]): boolean {
  return parts.some(isTerminalFinish);
}
