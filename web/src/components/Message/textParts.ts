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

// isTerminallyFinished mirrors message.Message.IsFinished() server-side
// (internal/message/content.go): true only when Parts contains a Finish
// part whose Partial flag is false/absent. A message with no Finish part at
// all, or only a Partial one (written by the server's auto-checkpoint
// ticker mid-stream), is NOT terminally finished — it is still owned by an
// in-flight agent turn, which will overwrite any edit made to it via its
// next checkpoint or, unconditionally, its terminal write (task #590).
export function isTerminallyFinished(parts: ContentPart[]) {
  return parts.some(p => p.type === "finish" && !p.Partial);
}
