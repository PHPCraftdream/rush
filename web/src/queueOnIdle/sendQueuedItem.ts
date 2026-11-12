/** Seams of a per-item queued send. */
export interface QueuedItemDependencies<T, E = unknown> {
  take(sessionID: string, id: string): { item: T; index: number } | undefined;
  request(sessionID: string, item: T): Promise<unknown>;
  restore(sessionID: string, item: T, index: number, error: E): void;
  describeFailure(error: unknown): E;
}

/**
 * Takes one queued item, sends it, and restores it with a described failure if the request rejects.
 *
 * @param sessionID - session id
 * @param id - queued item id
 * @param deps - queue and transport seams
 */
export async function sendQueuedItem<T, E>(
  sessionID: string,
  id: string,
  deps: QueuedItemDependencies<T, E>,
): Promise<void> {
  const taken = deps.take(sessionID, id);
  if (!taken) return;
  try {
    await deps.request(sessionID, taken.item);
  } catch (error) {
    deps.restore(sessionID, taken.item, taken.index, deps.describeFailure(error));
  }
}
