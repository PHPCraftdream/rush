// Attachment payload budgeting; the pieces live in ./attachmentLimit, one export per file.
export { MAX_ATTACHMENT_PAYLOAD_BYTES } from "./attachmentLimit/MAX_ATTACHMENT_PAYLOAD_BYTES";
export { ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES } from "./attachmentLimit/ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES";
export { estimateAttachmentPayloadBytes } from "./attachmentLimit/estimateAttachmentPayloadBytes";
export type { AttachmentSizeInput } from "./attachmentLimit/estimateAttachmentPayloadBytes";
export { isAttachmentPayloadWithinLimit } from "./attachmentLimit/isAttachmentPayloadWithinLimit";
