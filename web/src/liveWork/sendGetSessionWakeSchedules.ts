import { ws } from "../net/client";
import { wakeSchedulesCarburetor } from "../state/wakeSchedules";
import { WAKE_SCHEDULES_GET_PREFIX } from "./WAKE_SCHEDULES_GET_PREFIX";

/**
 * Requests a fresh wake-schedule snapshot for a session.
 *
 * Fire-and-forget: the reply arrives as an ordinary session_wake_schedules push
 * keyed by its own SessionID field.
 *
 * @param sessionID - session id
 */
export function sendGetSessionWakeSchedules(sessionID: string): void {
  const id = WAKE_SCHEDULES_GET_PREFIX + crypto.randomUUID();
  wakeSchedulesCarburetor.trackRequest(sessionID, id);
  ws.send("get_session_wake_schedules", { sessionID }, id);
}
