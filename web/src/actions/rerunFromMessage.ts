import { ws } from "../net/client";
import { sessionsCarburetor } from "../state/sessions";
import { logClientEvent } from "../telemetry/logClientEvent";

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
