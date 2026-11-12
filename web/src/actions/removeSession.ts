import { requestTracker } from "../net/requestTracker";
import { sessionsCarburetor } from "../state/sessions";
import { removeSessionQueueAndBusy } from "./removeSessionQueueAndBusy";
import { removeSubAgentState } from "./removeSubAgentState";

/**
 * Removes a session row and every piece of per-session state that would otherwise outlive it.
 *
 * @param id - the removed session
 */
export function removeSession(id: string): void {
  sessionsCarburetor.remove(id);
  // Drop the ws-layer request bookkeeping with the row (task #698): the
  // stale-reply floor and live-event epochs only matter while replies for
  // this session can still arrive and be applied.
  requestTracker.forgetSession(id);
  // A session that no longer exists can never receive the
  // agent_busy=false that flushes its queue (task #726).
  removeSessionQueueAndBusy(id);
  // Sub-agent state, delete tombstones and block breaks are keyed
  // independently of the sessions list: clear them with the row too, or they
  // grow once per session ever seen for the tab lifetime (task #727).
  removeSubAgentState(id);
}
