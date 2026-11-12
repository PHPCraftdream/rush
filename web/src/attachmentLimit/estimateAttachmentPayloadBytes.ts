import { ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES } from "./ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES.ts";

/** What the budget needs to know of one attachment: its raw size, or its already-encoded data. */
export interface AttachmentSizeInput {
  size?: number;
  data?: string;
}

/**
 * Size of a string as it appears inside a JSON document, without the quotes.
 *
 * @param value - the string to encode
 */
const jsonStringBytes = (value: string): number => new TextEncoder().encode(JSON.stringify(value)).byteLength - 2;

/**
 * Adds one attachment's encoded size to a running total.
 *
 * @param total - bytes so far
 * @param attachment - the attachment to add
 */
const addAttachmentBytes = (total: number, attachment: AttachmentSizeInput): number => {
  const encodedSize =
    attachment.data !== undefined ? jsonStringBytes(attachment.data) : 4 * Math.ceil((attachment.size ?? 0) / 3);
  return total + encodedSize;
};

/**
 * Estimates a complete JSON WebSocket frame.
 *
 * The server's read limit is 20 MiB (internal/server/hub.go:maxMessageSize);
 * keep a 1 MiB margin and reserve 64 KiB of that budget for JSON metadata and
 * framing. WebSocket sends JSON, so JSON escapes (notably control characters)
 * are accounted for rather than only the UTF-8 bytes of the original text.
 *
 * @param text - prompt text
 * @param attachments - attachments riding on the frame
 */
export function estimateAttachmentPayloadBytes(text: string, attachments: readonly AttachmentSizeInput[]): number {
  return jsonStringBytes(text) + attachments.reduce(addAttachmentBytes, 0) + ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES;
}
