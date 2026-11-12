import { messagesCarburetor } from "../state/messages";
import { sessionsCarburetor } from "../state/sessions";

/**
 * Clears every piece of state that outlives a removed session row (task #727).
 *
 * The sub-agent registration and transcript keyed by the id itself, plus those
 * of every sub-agent session parented by it: the server cascades sub-session
 * deletion with the parent (internal/server/handlers_sessions.go
 * handleDeleteSession), so a removed parent strands its whole branch. Also the
 * per-session delete tombstones and whichever message-block breaks can still be
 * attributed. Called from removeSession (live deletion) and the sessions_list
 * handler in connectWS (deletion that happened while this tab was offline).
 *
 * @param id - the removed session
 */
export function removeSubAgentState(id: string): void {
  messagesCarburetor.removeSubAgentState(id, sessionsCarburetor.getData().activeSessionID === id);
}
