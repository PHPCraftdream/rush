import { LIVE_WORK_REQUEST_PREFIX } from "./LIVE_WORK_REQUEST_PREFIX";

/**
 * True if the id belongs to a get_session_live_work request.
 *
 * @param id - correlation id of a reply
 */
export function isLiveWorkRequestID(id: string | undefined): boolean {
  return !!id && id.startsWith(LIVE_WORK_REQUEST_PREFIX);
}
