import { ws } from "../net/client";

/**
 * Replaces a message text.
 *
 * @param messageID - message id
 * @param content - the new text
 */
export function updateMessageContent(messageID: string, content: string): void {
  ws.send("update_message_content", { messageID, content });
}
