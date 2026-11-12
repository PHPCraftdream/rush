import type { WireAttachment } from "../../store";

/**
 * Builds the frame payload of a send; attachments ride along only when there are any.
 *
 * @param sessionID - target session
 * @param content - message text
 * @param attachments - wire attachments
 */
export function buildSendPayload(
  sessionID: string,
  content: string,
  attachments: readonly WireAttachment[],
): Record<string, unknown> {
  const payload: Record<string, unknown> = { sessionID, content };
  if (attachments.length > 0) {
    payload.attachments = attachments;
  }
  return payload;
}
