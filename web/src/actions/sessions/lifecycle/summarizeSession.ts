import { ws } from "../../../backend/ws/client";

/**
 * Asks the server to compact a session.
 *
 * @param sessionID - session id
 */
export function summarizeSession(sessionID: string): void {
  ws.send("summarize_session", { sessionID });
}
