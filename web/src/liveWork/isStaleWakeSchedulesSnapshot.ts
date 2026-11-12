import { wakeSchedulesCarburetor } from "../state/wakeSchedules";
import { isStaleWakeSnapshot } from "../wakeSnapshot/isStaleWakeSnapshot";
import { isWakeSchedulesCancelRequestID } from "./isWakeSchedulesCancelRequestID";
import { isWakeSchedulesGetRequestID } from "./isWakeSchedulesGetRequestID";

/**
 * Whether an id belongs to a tracked wake-schedules request (get or cancel).
 *
 * @param id - correlation id
 */
const isTrackedRequest = (id: string): boolean => isWakeSchedulesGetRequestID(id) || isWakeSchedulesCancelRequestID(id);

/**
 * Whether a tagged response was superseded by a later wake-schedule request.
 *
 * Untagged pushes and replies from older clients remain accepted.
 *
 * @param sessionID - session of the snapshot
 * @param requestID - correlation id of the reply
 */
export function isStaleWakeSchedulesSnapshot(sessionID: string | undefined, requestID: string | undefined): boolean {
  return isStaleWakeSnapshot(wakeSchedulesCarburetor.latestRequests(), sessionID, requestID, isTrackedRequest);
}
