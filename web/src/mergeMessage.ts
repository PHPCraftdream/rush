import type { ContentPart, Message } from "./types";

/**
 * The parts of one kind, narrowed to that kind.
 *
 * @param parts - the message parts
 * @param kind - the part type to keep
 */
const partsOfKind = <T extends ContentPart["type"]>(
  parts: readonly ContentPart[],
  kind: T,
): Extract<ContentPart, { type: T }>[] => parts.filter((p): p is Extract<ContentPart, { type: T }> => p.type === kind);

/**
 * The text a part carries toward the accumulated total, or "".
 *
 * @param p - the part
 */
const partText = (p: ContentPart): string => {
  if (p.type === "thinking") return p.Thinking;
  if (p.type === "text") return p.Text;
  return "";
};

/**
 * The concatenated thinking or text of a message.
 *
 * @param parts - the message parts
 * @param kind - which accumulating kind to total
 */
const total = (parts: readonly ContentPart[], kind: "thinking" | "text"): string =>
  parts
    .filter((p) => p.type === kind)
    .map(partText)
    .join("");

/**
 * Whether a part is a final (non-partial) finish marker.
 *
 * @param p - the part
 */
const isTerminalFinish = (p: ContentPart): boolean => p.type === "finish" && !p.Partial;

/**
 * Whether the message carries a final finish part.
 *
 * @param parts - the message parts
 */
const terminal = (parts: readonly ContentPart[]): boolean => parts.some(isTerminalFinish);

/**
 * Keeps regressed accumulating content while respecting the incoming structural part order.
 *
 * @param existing - the row the client already holds
 * @param incoming - the pushed row
 */
export function mergePreserveContent(existing: Message, incoming: Message): Message {
  if (terminal(incoming.Parts)) return incoming;
  const thinkingRegressed = total(incoming.Parts, "thinking").length < total(existing.Parts, "thinking").length;
  const textRegressed = total(incoming.Parts, "text").length < total(existing.Parts, "text").length;
  if (!thinkingRegressed && !textRegressed) return incoming;

  const regressedKinds = new Set<ContentPart["type"]>();
  if (thinkingRegressed) regressedKinds.add("thinking");
  if (textRegressed) regressedKinds.add("text");
  const rebuilt: ContentPart[] = [];
  const emittedCounts = new Map<ContentPart["type"], number>();
  for (const part of incoming.Parts) {
    if (!regressedKinds.has(part.type)) {
      rebuilt.push(part);
      continue;
    }
    const index = emittedCounts.get(part.type) ?? 0;
    const protectedParts = partsOfKind(existing.Parts, part.type);
    if (index < protectedParts.length) {
      rebuilt.push(protectedParts[index]);
      emittedCounts.set(part.type, index + 1);
    } else rebuilt.push(part);
  }
  for (const kind of regressedKinds) {
    const protectedParts = partsOfKind(existing.Parts, kind);
    const emittedCount = emittedCounts.get(kind) ?? 0;
    if (emittedCount >= protectedParts.length) continue;
    const firstExistingIndex = existing.Parts.findIndex((part) => part.type === kind);
    const insertAtIncomingIndex = incoming.Parts.findIndex((part) => {
      const existingIndex = existing.Parts.findIndex(
        (candidate) => candidate.type === part.type && JSON.stringify(candidate) === JSON.stringify(part),
      );
      return existingIndex !== -1 && existingIndex > firstExistingIndex;
    });
    const insertAt =
      insertAtIncomingIndex === -1
        ? Math.min(Math.max(firstExistingIndex, 0), rebuilt.length)
        : Math.min(insertAtIncomingIndex, rebuilt.length);
    rebuilt.splice(insertAt, 0, ...protectedParts.slice(emittedCount));
  }
  return { ...incoming, Parts: rebuilt };
}
