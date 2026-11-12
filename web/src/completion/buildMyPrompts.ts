import type { Message } from "../types";
import type { MyPromptItem } from "./completionTypes";
import { isComposerTypedMessage } from "./isComposerTypedMessage.ts";
import { partsToText } from "./partsToText.ts";

/**
 * Builds the composer prompt recall list from the transcript, newest first.
 *
 * Newest first matches "press up to get the previous prompt".
 *
 * @param msgs - the transcript
 * @param isNotice - content-pattern notice predicate
 */
export function buildMyPrompts(msgs: readonly Message[], isNotice: (m: Message) => boolean): MyPromptItem[] {
  const out: MyPromptItem[] = [];
  for (const m of msgs) {
    if (!isComposerTypedMessage(m, isNotice)) continue;
    const text = partsToText(m.Parts as unknown as Array<{ type: string; Text?: string }>).trim();
    if (!text) continue;
    out.push({ id: m.ID, text });
  }
  return out.reverse();
}
