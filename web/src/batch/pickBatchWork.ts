import type { BatchWorkState } from "../state/BatchWorkCarburetor";

/**
 * One session's batch state, or null before any snapshot.
 *
 * Takes the table as an argument rather than reading the store: callers pass
 * the value their store hook returned, so the React Compiler sees the reactive
 * input and cannot memoize the result across unrelated store updates.
 *
 * @param map - the batch table
 * @param sessionID - session id
 */
export function pickBatchWork(
  map: ReadonlyMap<string, BatchWorkState>,
  sessionID: string | null,
): BatchWorkState | null {
  if (!sessionID) return null;
  return map.get(sessionID) ?? null;
}
