import { messagesCarburetor } from "../state/messages";

/**
 * Gathers the agent response to one user prompt: thinking + text of every assistant message of the turn.
 *
 * Collection stops at the next user message (or end of conversation). Tool
 * calls and tool results are excluded: the operator wants the agent prose, not
 * its action log. Accepts EITHER a user message ID (canonical) OR any
 * assistant/tool message ID belonging to that turn: it walks backwards to find
 * the turn user message either way, so a "Copy all" button can live on the
 * agent final message as well as on the user prompt. Returns "" if the user
 * message has no agent response yet.
 *
 * @param anyMessageID - a message of the turn
 */
export function collectTurnContent(anyMessageID: string): string {
  const msgs = messagesCarburetor.getData().messages;
  let startIdx = msgs.findIndex((m) => m.ID === anyMessageID);
  if (startIdx === -1) return "";
  // If we were handed a non-user message, walk back to the turn user message.
  if (msgs[startIdx].Role !== "user") {
    let walk = startIdx;
    while (walk > 0 && msgs[walk].Role !== "user") walk--;
    if (msgs[walk].Role !== "user") return "";
    startIdx = walk;
  }

  const chunks: string[] = [];
  for (let i = startIdx + 1; i < msgs.length; i++) {
    const m = msgs[i];
    if (m.Hidden) continue;
    if (m.Role === "user") break; // next user turn: stop
    if (m.Role !== "assistant") continue; // skip tool-role messages
    for (const p of m.Parts) {
      if (p.type === "thinking") {
        const t = (p as { type: "thinking"; Thinking: string }).Thinking;
        if (t && t.trim()) chunks.push(`<thinking>\n${t}\n</thinking>`);
      } else if (p.type === "text") {
        const t = (p as { type: "text"; Text: string }).Text;
        if (t && t.trim()) chunks.push(t);
      }
    }
  }
  return chunks.join("\n\n");
}
