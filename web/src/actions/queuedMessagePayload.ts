import type { QueuedMessage } from "../state/QueueCarburetor";

/**
 * Builds the wire payload of one queued message.
 *
 * @param sessionID - session id
 * @param item - the queued message
 */
export function queuedMessagePayload(sessionID: string, item: QueuedMessage): Record<string, unknown> {
  const payload: Record<string, unknown> = { sessionID, content: item.content };
  if (item.attachments && item.attachments.length > 0) {
    payload.attachments = item.attachments;
  }
  return payload;
}
