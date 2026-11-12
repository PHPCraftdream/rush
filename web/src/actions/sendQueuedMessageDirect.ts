import { ws } from "../net/client";
import { queueCarburetor } from "../state/queue";
import { queuedMessagePayload } from "./queuedMessagePayload";

/**
 * Sends one queued message via the normal send_message path, for a session that is IDLE (task #1057).
 *
 * inject/interrupt only make sense against a running turn. Fire-and-forget,
 * exactly like the composer Send button when idle (ChatInput.tsx): send_message
 * runs the whole turn server-side and replies only on failure (a bare error, no
 * request/response pairing on success), so wsRequest would time out on every
 * ordinary long-running success. The only failure observable here is the frame
 * never reaching the socket at all (offline outbox full); that is what gets
 * restored. Never auto-resent from here: a persistent failure would loop.
 *
 * @param sessionID - session id
 * @param id - queued message id
 */
export function sendQueuedMessageDirect(sessionID: string, id: string): void {
  const taken = queueCarburetor.take(sessionID, id);
  if (!taken) return;
  if (!ws.sendQueued("send_message", queuedMessagePayload(sessionID, taken.item))) {
    const reason = "Not connected — message was not sent.";
    queueCarburetor.restoreItem(sessionID, taken.item, taken.index, reason, "rejected");
  }
}
