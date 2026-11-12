import { flushQueueOnIdle } from "../queueOnIdle/flushQueueOnIdle";
import { queueCarburetor } from "../state/queue";
import type { WireAttachment } from "../state/QueueCarburetor";

/**
 * Drains the session queue.
 *
 * @param id - session id
 */
const dequeueQueue = (id: string): ReturnType<typeof queueCarburetor.dequeueAll> => queueCarburetor.dequeueAll(id);

/**
 * Puts a drained payload back after a failed send.
 *
 * @param id - session id
 * @param content - the drained text
 * @param attachments - the drained attachments
 */
const restoreQueue = (id: string, content: string, attachments: unknown[]): void =>
  queueCarburetor.enqueue(id, content, attachments as WireAttachment[]);

/**
 * Sends the session queued messages as one combined message when its agent goes idle (task #1057).
 *
 * @param sessionID - session id
 * @param send - transport; false means the frame could not be sent or parked
 */
export function flushQueuedMessagesOnIdle(
  sessionID: string,
  send: (payload: Record<string, unknown>) => boolean,
): void {
  flushQueueOnIdle(sessionID, {
    dequeue: dequeueQueue,
    send,
    restore: restoreQueue,
  });
}
