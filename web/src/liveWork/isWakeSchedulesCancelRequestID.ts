import { WAKE_SCHEDULES_CANCEL_PREFIX } from "./WAKE_SCHEDULES_CANCEL_PREFIX";

/**
 * True if the id belongs to a wake-schedules CANCEL request.
 *
 * @param id - correlation id of a reply
 */
export function isWakeSchedulesCancelRequestID(id: string | undefined): boolean {
  return !!id && id.startsWith(WAKE_SCHEDULES_CANCEL_PREFIX);
}
