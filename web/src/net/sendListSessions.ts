import { ws } from "./client";
import { requestTracker } from "./requestTracker";

/**
 * Sends list_sessions tagged with a fresh correlation id (mirrors sendLoadMessages).
 *
 * Returns whether the frame actually left the socket (see WSClient.send) so
 * reconnect callers can report a drop instead of silently ignoring it
 * (task #727); the pollers re-issue it every 5s regardless.
 */
export function sendListSessions(): boolean {
  const id = requestTracker.beginListSessions();
  return ws.send("list_sessions", undefined, id);
}
