import { ws } from "../net/client";

/**
 * Deletes one message.
 *
 * @param messageID - message id
 */
export function deleteMessage(messageID: string): void {
  ws.send("delete_message", { messageID });
}
