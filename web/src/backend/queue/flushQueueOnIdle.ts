/** What a busy=false flush drains from the queue. */
export interface IdleQueuePayload {
  content: string;
  attachments: unknown[];
}

/** Seams of the busy=false flush. */
export interface IdleQueueDependencies<T extends IdleQueuePayload> {
  dequeue(sessionID: string): T | undefined;
  send(payload: { sessionID: string; content: string; attachments?: unknown[] }): boolean;
  restore(sessionID: string, content: string, attachments: unknown[]): void;
}

/**
 * Production busy=false queue drain, send, and failed-send restoration seam.
 *
 * @param sessionID - session id
 * @param deps - queue and transport seams
 */
export function flushQueueOnIdle<T extends IdleQueuePayload>(sessionID: string, deps: IdleQueueDependencies<T>): void {
  const flushed = deps.dequeue(sessionID);
  if (!flushed) return;
  const payload: { sessionID: string; content: string; attachments?: unknown[] } = {
    sessionID,
    content: flushed.content,
  };
  if (flushed.attachments.length > 0) payload.attachments = flushed.attachments;
  if (!deps.send(payload)) deps.restore(sessionID, flushed.content, flushed.attachments);
}
