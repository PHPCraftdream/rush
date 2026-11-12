import { ws } from "../net/client";
import { LIVE_WORK_REQUEST_PREFIX } from "./LIVE_WORK_REQUEST_PREFIX";

/**
 * Requests a fresh live-work snapshot for a session.
 *
 * Fire-and-forget: the reply arrives as an ordinary session_live_work push,
 * keyed by its own SessionID field, so nothing here needs to correlate by
 * request id for the success path.
 *
 * @param sessionID - session id
 */
export function sendGetSessionLiveWork(sessionID: string): void {
  const id = LIVE_WORK_REQUEST_PREFIX + crypto.randomUUID();
  ws.send("get_session_live_work", { sessionID }, id);
}
