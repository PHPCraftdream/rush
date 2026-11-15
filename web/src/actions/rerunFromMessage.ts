import { ws } from "../backend/ws/client";
import { sessionsCarburetor } from "../state/sessions/sessions";
import { logClientEvent } from "../backend/telemetry/logClientEvent";

/**
 * Re-runs the turn that starts at a message in the active session.
 *
 * @param messageID - message id
 */
export function rerunFromMessage(messageID: string): void {
  const sessionID = sessionsCarburetor.getData().activeSessionID;
  if (!sessionID) return;
  logClientEvent("rerun_message", { sessionID, messageID });
  ws.send("rerun_message", { messageID });
}
