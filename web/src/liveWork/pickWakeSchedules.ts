import type { WakeScheduleItem } from "../types";

const EMPTY_SCHEDULES: WakeScheduleItem[] = [];

/**
 * One session wake schedules, or an empty list.
 *
 * Takes the table explicitly, for the same React Compiler reason as pickLiveWork.
 *
 * @param map - the schedule table
 * @param sessionID - session id
 */
export function pickWakeSchedules(
  map: ReadonlyMap<string, WakeScheduleItem[]>,
  sessionID: string | null,
): WakeScheduleItem[] {
  if (!sessionID) return EMPTY_SCHEDULES;
  return map.get(sessionID) ?? EMPTY_SCHEDULES;
}
