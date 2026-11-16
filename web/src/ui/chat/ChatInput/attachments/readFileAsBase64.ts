import type { PendingAttachment } from "./PendingAttachment";

/**
 * Reads a file into a base64 pending attachment.
 *
 * @param file - the picked file
 */
export async function readFileAsBase64(file: File): Promise<PendingAttachment> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      const arrayBuffer = reader.result as ArrayBuffer;
      const bytes = new Uint8Array(arrayBuffer);
      let binary = "";
      bytes.forEach((b) => (binary += String.fromCharCode(b)));
      resolve({
        fileName: file.name,
        mimeType: file.type || "application/octet-stream",
        data: btoa(binary),
        size: file.size,
      });
    };
    reader.onerror = reject;
    reader.readAsArrayBuffer(file);
  });
}
