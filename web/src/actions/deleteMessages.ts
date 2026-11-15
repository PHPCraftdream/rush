import { ws } from "../backend/ws/client";

/**
 * Deletes several messages.
 *
 * @param messageIDs - message ids
 */
export function deleteMessages(messageIDs: string[]): void {
  ws.send("delete_messages", { messageIDs });
}
