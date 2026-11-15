import type { ContentPart, Message } from "../types";

/**
 * Whether a part is a text part.
 *
 * @param part - the part
 */
const isTextPart = (part: ContentPart): part is Extract<ContentPart, { type: "text" }> => part.type === "text";

/**
 * The text of a text part.
 *
 * @param part - the text part
 */
const textOf = (part: Extract<ContentPart, { type: "text" }>): string => part.Text;

/**
 * The message's text parts joined by newlines.
 *
 * @param message - the message
 */
export function messageText(message: Message): string {
  return message.Parts.filter(isTextPart).map(textOf).join("\n");
}
