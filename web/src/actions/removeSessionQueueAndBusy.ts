import { busyCarburetor } from "../state/busy";
import { queueCarburetor } from "../state/queue";

/**
 * Drops the queued messages and busy flag for a session that no longer exists (task #726).
 *
 * A removed session can never receive the agent_busy=false that flushes its
 * queue, so keeping either would strand the other. Called from removeSession
 * (live deletion) and the sessions_list handler in connectWS (deletion that
 * happened while this tab was offline).
 *
 * @param id - the removed session
 */
export function removeSessionQueueAndBusy(id: string): void {
  queueCarburetor.removeSession(id);
  if (busyCarburetor.isBusy(id)) busyCarburetor.setBusy(id, false);
}
