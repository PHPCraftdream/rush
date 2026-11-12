import type { SessionLiveWork } from "../state/LiveWorkCarburetor";

const EMPTY_LIVE_WORK: SessionLiveWork = { commands: [], agents: [] };

/**
 * One session live work; sessions with no snapshot yet (or never requested) read as empty, not absent.
 *
 * Takes the table as an explicit argument rather than reading the store, so
 * callers pass the snapshot their component already subscribed to and the
 * render stays a pure function of tracked reads.
 *
 * @param map - the snapshot table
 * @param sessionID - session id
 */
export function pickLiveWork(map: ReadonlyMap<string, SessionLiveWork>, sessionID: string | null): SessionLiveWork {
  if (!sessionID) return EMPTY_LIVE_WORK;
  return map.get(sessionID) ?? EMPTY_LIVE_WORK;
}
