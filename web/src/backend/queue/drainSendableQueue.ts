/**
 * Whether a queued item has not failed a per-item send.
 *
 * @param item - the queued item
 */
const isClean = (item: { failureState?: string }): boolean => !item.failureState;

/**
 * Whether a queued item already failed a per-item send.
 *
 * @param item - the queued item
 */
const isFailed = (item: { failureState?: string }): boolean => !!item.failureState;

/**
 * Splits a queue into items safe to send now and items that already failed a per-item send.
 *
 * @param items - the queued items
 */
export function drainSendableQueue<T extends { content: string; attachments?: unknown[]; failureState?: string }>(
  items: readonly T[],
): { sendable: T[]; retained: T[] } {
  return {
    sendable: items.filter(isClean),
    retained: items.filter(isFailed),
  };
}
