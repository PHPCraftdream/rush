import { WAKE_SCHEDULES_GET_PREFIX } from "./WAKE_SCHEDULES_GET_PREFIX";

/**
 * True if the id belongs to a wake-schedules GET request.
 *
 * @param id - correlation id of a reply
 */
export function isWakeSchedulesGetRequestID(id: string | undefined): boolean {
  return !!id && id.startsWith(WAKE_SCHEDULES_GET_PREFIX);
}
