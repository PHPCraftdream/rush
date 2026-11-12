import { ws } from "../net/client";

/**
 * Deletes one part of a message.
 *
 * @param messageID - message id
 * @param partIndex - part index
 */
export function deleteMessagePart(messageID: string, partIndex: number): void {
  ws.send("delete_message_part", { messageID, partIndex });
}
