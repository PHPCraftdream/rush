import type { WireAttachment } from "../../store";
import type { PendingAttachment } from "./PendingAttachment";

/**
 * Drops the local-only size from one attachment.
 *
 * @param a - the pending attachment
 */
const toWire = (a: PendingAttachment): WireAttachment => ({
  fileName: a.fileName,
  mimeType: a.mimeType,
  data: a.data,
});

/**
 * Strips the local-only `size` field — the wire shape is fileName/mimeType/data.
 *
 * @param atts - pending attachments
 */
export function toWireAttachments(atts: readonly PendingAttachment[]): WireAttachment[] {
  return atts.map(toWire);
}
