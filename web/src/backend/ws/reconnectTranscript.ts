/**
 * Reloads the active transcript after a reconnect.
 *
 * Reconnect may have missed message_created/updated/deleted pushes, so the
 * transcript is loaded directly rather than relying on sessions_list routing.
 *
 * @param activeSessionID - the active session, or null
 * @param loadMessages - sends load_messages for a session
 */
export function reloadReconnectTranscript(
  activeSessionID: string | null,
  loadMessages: (sessionID: string) => void,
): void {
  if (activeSessionID) loadMessages(activeSessionID);
}
