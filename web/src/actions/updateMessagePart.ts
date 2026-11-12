import { ws } from "../net/client";

/**
 * Replaces the content of one part of a message.
 *
 * @param messageID - message id
 * @param partIndex - part index
 * @param content - the new content
 */
export function updateMessagePart(messageID: string, partIndex: number, content: string): void {
  ws.send("update_message_part", { messageID, partIndex, content });
}
