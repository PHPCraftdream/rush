import { ws } from "../net/client";
import { configCarburetor } from "../state/config";
import type { WireAttachment } from "../state/QueueCarburetor";
import { sessionsCarburetor } from "../state/sessions";

/**
 * Sends a message through the session (or the configured) fast model.
 *
 * Mirrors the backend SendMessagePayload: the attachments are one file per entry.
 *
 * @param sessionID - session id
 * @param content - message text
 * @param attachments - attachments riding on the frame
 */
export function sendWithFastModel(sessionID: string, content: string, attachments?: WireAttachment[]): void {
  const config = configCarburetor.getData().config;
  const sess = sessionsCarburetor.findSession(sessionID);
  let fastModel: { provider: string; model: string } | undefined;
  if (sess && sess.FastModelID) {
    fastModel = { provider: sess.FastModelProvider, model: sess.FastModelID };
  } else if (config?.models?.fast) {
    fastModel = { provider: config.models.fast.Provider, model: config.models.fast.Model };
  }
  const payload: Record<string, unknown> = { sessionID, content };
  if (fastModel) {
    payload.smartModel = fastModel;
  }
  if (attachments && attachments.length > 0) {
    payload.attachments = attachments;
  }
  ws.send("send_message", payload);
}
