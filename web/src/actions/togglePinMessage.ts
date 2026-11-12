import { ws } from "../net/client";

/**
 * Pins or unpins a message.
 *
 * @param messageID - message id
 * @param pinned - the new pin state
 */
export function togglePinMessage(messageID: string, pinned: boolean): void {
  ws.send("toggle_pin_message", { messageID, pinned });
}
