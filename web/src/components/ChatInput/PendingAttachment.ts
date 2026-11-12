/** A file picked in the composer, held until the message goes out. */
export interface PendingAttachment {
  fileName: string;
  mimeType: string;
  /** Base64. */
  data: string;
  size: number;
}
