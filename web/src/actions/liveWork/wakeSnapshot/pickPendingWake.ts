/**
 * Keeps a pending cancellation only while its schedule is still active in the snapshot.
 *
 * @param items - schedules of the snapshot
 * @param pending - the schedule awaiting cancellation
 */
export function pickPendingWake<T extends { id: string; state: string }>(
  items: readonly T[],
  pending: T | null,
): T | null {
  return pending && items.some((item) => item.id === pending.id && item.state === "active") ? pending : null;
}
