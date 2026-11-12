import { ws } from "../net/client";
import { wakeSchedulesCarburetor } from "../state/wakeSchedules";
import { WAKE_SCHEDULES_CANCEL_PREFIX } from "./WAKE_SCHEDULES_CANCEL_PREFIX";

/**
 * Cancels one of the session own wake schedules.
 *
 * The server replies with a fresh session_wake_schedules snapshot; an
 * unknown/foreign scheduleID is an explicit error reply (surfaced by the
 * dialog inline error).
 *
 * @param sessionID - session id
 * @param scheduleID - schedule to cancel
 */
export function sendCancelWakeSchedule(sessionID: string, scheduleID: string): void {
  const id = WAKE_SCHEDULES_CANCEL_PREFIX + crypto.randomUUID();
  wakeSchedulesCarburetor.trackRequest(sessionID, id);
  ws.send("cancel_wake_schedule", { sessionID, scheduleID }, id);
}
