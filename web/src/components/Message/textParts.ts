// Text/thinking extraction helpers shared by the message content components.
// Pure code move from the former components/Message.tsx.

import type { ContentPart } from "../../types";

// ── Tiny utilities ────────────────────────────────────────────────────────────

export function extractText(parts: ContentPart[]) {
  return parts.filter(p => p.type === "text").map(p => (p as any).Text).join("\n");
}

export function extractThinking(parts: ContentPart[]) {
  return parts.filter(p => p.type === "thinking").map(p => (p as any).Thinking).join("\n");
}
