// Text extraction helper shared by the message content components.

import type { ContentPart } from "../../types";

/**
 * Whether a message part is a text part.
 *
 * @param part - the message part
 */
const isText = (part: ContentPart): boolean => part.type === "text";

/**
 * Reads the text of a text part.
 *
 * @param part - a text part
 */
const textOf = (part: ContentPart): string => (part as { Text: string }).Text;

/**
 * Joins the text parts of a message, one per line.
 *
 * @param parts - the message parts
 */
export function extractText(parts: readonly ContentPart[]): string {
  return parts.filter(isText).map(textOf).join("\n");
}
