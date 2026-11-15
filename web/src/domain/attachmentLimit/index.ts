// Attachment payload budgeting; the pieces live in ./attachmentLimit, one export per file.
export { MAX_ATTACHMENT_PAYLOAD_BYTES } from "./MAX_ATTACHMENT_PAYLOAD_BYTES";
export { ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES } from "./ATTACHMENT_OVERHEAD_ALLOWANCE_BYTES";
export { estimateAttachmentPayloadBytes } from "./estimateAttachmentPayloadBytes";
export type { AttachmentSizeInput } from "./estimateAttachmentPayloadBytes";
export { isAttachmentPayloadWithinLimit } from "./isAttachmentPayloadWithinLimit";
