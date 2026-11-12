import { ws } from "../net/client";

/**
 * Replaces a thinking part of a message.
 *
 * @param messageID - message id
 * @param thinking - the new thinking text
 * @param partIndex - which thinking part; omitted means the first
 */
export function updateMessageThinking(messageID: string, thinking: string, partIndex?: number): void {
  ws.send("update_message_thinking", { messageID, thinking, partIndex });
}
