// Groups message parts into visual blocks for the zebra-stripe layout.

import type { ContentPart } from "../../types";

export type BlockKind = "thinking" | "text" | "tool" | "other";

/** One zebra-stripe block: a run of same-kind parts. */
export interface VisualBlock {
  kind: BlockKind;
  items: { part: ContentPart; idx: number }[];
  thinkingDone: boolean;
}

/**
 * Maps a part to the kind of block it belongs to.
 *
 * @param part - the message part
 */
function classifyPart(part: ContentPart): BlockKind {
  switch (part.type) {
    case "thinking":    return "thinking";
    case "text":        return "text";
    case "tool_call":
    case "tool_result": return "tool";
    default:            return "other";
  }
}

/**
 * Whether a block marks the end of the thinking before it.
 *
 * @param block - a block following a thinking block
 */
function endsThinking(block: VisualBlock): boolean {
  return block.kind === "text" || block.kind === "other";
}

/**
 * Splits parts into runs of the same kind; a break index always starts a new run.
 *
 * @param parts - the message parts in order
 * @param breaks - part indices where a new block must start
 */
export function groupPartsIntoBlocks(parts: readonly ContentPart[], breaks: ReadonlySet<number>): VisualBlock[] {
  const blocks: VisualBlock[] = [];
  let cur: VisualBlock | null = null;

  for (let i = 0; i < parts.length; i++) {
    const kind = classifyPart(parts[i]);
    if (!cur || cur.kind !== kind || breaks.has(i)) {
      cur = { kind, items: [], thinkingDone: false };
      blocks.push(cur);
    }
    cur.items.push({ part: parts[i], idx: i });
  }

  for (let b = 0; b < blocks.length; b++) {
    if (blocks[b].kind === "thinking") {
      blocks[b].thinkingDone = blocks.slice(b + 1).some(endsThinking);
    }
  }

  return blocks;
}
