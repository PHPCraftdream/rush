import type { SessionLiveWork } from "../state/LiveWorkCarburetor";

const EMPTY_LIVE_WORK: SessionLiveWork = { commands: [], agents: [] };

/**
 * One session live work; sessions with no snapshot yet (or never requested) read as empty, not absent.
 *
 * Takes the table as an explicit argument rather than reading the store. This
 * is required, not stylistic: the project React Compiler babel plugin
 * (rsbuild.config.ts) memoizes a component derived values based on its
 * recognized reactive inputs (props/state/hook return values) only. A plain
 * function that reaches into the store outside a hook return value is
 * invisible to that analysis: the compiler treated `pickLiveWork(sessionID)`
 * as a pure function of `sessionID` alone and memoized the JSX consuming its
 * result across unrelated store updates, so a fresh snapshot landed in the
 * store but never reached the rendered tab bar. Callers MUST pass the value a
 * store hook returned here.
 *
 * @param map - the snapshot table
 * @param sessionID - session id
 */
export function pickLiveWork(map: ReadonlyMap<string, SessionLiveWork>, sessionID: string | null): SessionLiveWork {
  if (!sessionID) return EMPTY_LIVE_WORK;
  return map.get(sessionID) ?? EMPTY_LIVE_WORK;
}
