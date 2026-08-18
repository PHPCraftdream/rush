// Groups message parts into visual blocks for the zebra-stripe layout.
// Pure code move from the former components/Message.tsx.

import type { ContentPart } from "../../types";

// ── Block grouping for zebra pattern ──────────────────────────────────────

export const EMPTY_BREAKS = new Set<number>();

type BlockKind = "thinking" | "text" | "tool" | "other";

interface VisualBlock {
  kind: BlockKind;
  items: { part: ContentPart; idx: number }[];
  thinkingDone: boolean;
}

function classifyPart(part: ContentPart): BlockKind {
  switch (part.type) {
    case "thinking":    return "thinking";
    case "text":        return "text";
    case "tool_call":
    case "tool_result": return "tool";
    default:            return "other";
  }
}

export function groupPartsIntoBlocks(parts: ContentPart[], breaks: Set<number>): VisualBlock[] {
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
      blocks[b].thinkingDone = blocks.slice(b + 1).some(
        bb => bb.kind === "text" || bb.kind === "other"
      );
    }
  }

  return blocks;
}
