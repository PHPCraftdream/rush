import { messagesCarburetor } from "../state/messages";
import { noticesCarburetor } from "../state/notices";
import { sessionsCarburetor } from "../state/sessions";

/**
 * Makes a session active: clears the transcript and agent error, and syncs the URL hash.
 *
 * The single choke point every activation path (hashchange, session_created,
 * sessions_list routing, Sidebar) funnels through.
 *
 * @param id - the session to show, or null for none
 */
export function setActiveSession(id: string | null): void {
  sessionsCarburetor.setActiveSessionID(id);
  messagesCarburetor.setMessages([]);
  noticesCarburetor.clearAgentError();
  if (id) {
    window.location.hash = `#/${id}`;
  } else {
    window.location.hash = "";
  }
}
