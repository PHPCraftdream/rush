import { ws } from "./client";
import { requestTracker } from "./requestTracker";

/**
 * Sends load_messages for a session, tagged with a fresh correlation id.
 *
 * That id becomes the only one whose reply is accepted for the session until a
 * newer load_messages supersedes it.
 *
 * @param sessionID - session id
 */
export function sendLoadMessages(sessionID: string): void {
  const id = requestTracker.beginLoadMessages(sessionID);
  ws.send("load_messages", { sessionID }, id);
}
