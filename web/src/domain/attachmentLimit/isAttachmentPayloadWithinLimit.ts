import { estimateAttachmentPayloadBytes } from "./estimateAttachmentPayloadBytes.ts";
import type { AttachmentSizeInput } from "./estimateAttachmentPayloadBytes.ts";
import { MAX_ATTACHMENT_PAYLOAD_BYTES } from "./MAX_ATTACHMENT_PAYLOAD_BYTES.ts";

/**
 * Whether prompt text plus attachments fit one WebSocket frame.
 *
 * @param text - prompt text
 * @param attachments - attachments riding on the frame
 */
export function isAttachmentPayloadWithinLimit(text: string, attachments: readonly AttachmentSizeInput[]): boolean {
  return estimateAttachmentPayloadBytes(text, attachments) <= MAX_ATTACHMENT_PAYLOAD_BYTES;
}
