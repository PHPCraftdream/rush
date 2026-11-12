/**
 * Concatenates the text parts of a message.
 *
 * @param parts - the message parts
 */
export function partsToText(parts: ReadonlyArray<{ type: string; Text?: string }>): string {
  let out = "";
  for (const p of parts) {
    if (p.type === "text" && p.Text) out += p.Text;
  }
  return out;
}
