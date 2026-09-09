import { ws } from "./ws";
import { $messages, $summarizeQueued, $activeSessionID, $config, $sessions, type WireAttachment } from "./store";
import { logClientEvent } from "./telemetry";

// Outbound commands: thin wrappers over the WebSocket protocol. Each one is
// fire-and-forget -- the server answers with a broadcast that the store's own
// reducers apply, so none of these mutate state directly. Split out of
// store.ts when the 1000-line file limit landed.

// setKeepAliveEnabled toggles the WebAudio keep-alive preference. The
// backend persists it to the global rush.json under
// options.keep_alive_enabled and broadcasts a fresh `config` event;
// useWS.ts reacts to that broadcast and starts/stops the local audio.
export function setKeepAliveEnabled(enabled: boolean) {
  ws.send("set_keep_alive", { enabled });
}

export function setProviderKey(providerID: string, apiKey: string) {
  ws.send("set_provider_key", { providerID, apiKey });
}

export function removeProviderKey(providerID: string) {
  ws.send("remove_provider_key", { providerID });
}

export function deleteMessage(messageID: string) {
  ws.send("delete_message", { messageID });
}

export function deleteMessages(messageIDs: string[]) {
  ws.send("delete_messages", { messageIDs });
}

export function updateMessageContent(messageID: string, content: string) {
  ws.send("update_message_content", { messageID, content });
}

export function updateMessageThinking(messageID: string, thinking: string) {
  ws.send("update_message_thinking", { messageID, thinking });
}

export function summarizeSession(sessionID: string) {
  ws.send("summarize_session", { sessionID });
}

export function cancelQueuedSummarize(sessionID: string) {
  ws.send("cancel_queued_summarize", { sessionID });
}

export function setSummarizeQueued(sessionID: string, queued: boolean) {
  const s = new Set($summarizeQueued.get());
  if (queued) s.add(sessionID);
  else s.delete(sessionID);
  $summarizeQueued.set(s);
}

export function deleteMessagePart(messageID: string, partIndex: number) {
  ws.send("delete_message_part", { messageID, partIndex });
}

export function updateMessagePart(messageID: string, partIndex: number, content: string) {
  ws.send("update_message_part", { messageID, partIndex, content });
}

export function togglePinMessage(messageID: string, pinned: boolean) {
  ws.send("toggle_pin_message", { messageID, pinned });
}

export function rerunFromMessage(messageID: string) {
  const sessionID = $activeSessionID.get();
  if (!sessionID) return;
  logClientEvent("rerun_message", { sessionID, messageID });
  ws.send("rerun_message", { messageID });
}

// collectTurnContent gathers the agent's response to one user prompt:
// thinking + text from every assistant message that follows the given user
// message, stopping at the next user message (or end of conversation). Tool
// calls and tool results are excluded — the operator wants the agent's prose,
// not its action log.
//
// Accepts EITHER a user message ID (canonical) OR any assistant/tool message
// ID belonging to that turn — the function walks backwards to find the turn's
// user message either way. This lets a "Copy all" button live on the agent's
// final message just as well as on the user's prompt.
//
// Returns "" if the user message has no agent response yet.
export function collectTurnContent(anyMessageID: string): string {
  const msgs = $messages.get();
  let startIdx = msgs.findIndex((m) => m.ID === anyMessageID);
  if (startIdx === -1) return "";
  // If we were handed a non-user message, walk back to the turn's user message.
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
    if (m.Role === "user") break; // next user turn — stop
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

// Wire shape of one file attachment in a send_message / inject_message
// frame. Mirrors the backend's SendMessagePayload.Attachments entry.
export function sendWithFastModel(
  sessionID: string,
  content: string,
  attachments?: WireAttachment[]
) {
  const config = $config.get();
  const sess = $sessions.get().find((s) => s.ID === sessionID);
  let fastModel: { provider: string; model: string } | undefined;
  if (sess && sess.FastModelID) {
    fastModel = { provider: sess.FastModelProvider, model: sess.FastModelID };
  } else if (config?.models?.fast) {
    fastModel = { provider: config.models.fast.Provider, model: config.models.fast.Model };
  }
  const payload: Record<string, unknown> = { sessionID, content };
  if (fastModel) {
    payload.smartModel = fastModel;
  }
  if (attachments && attachments.length > 0) {
    payload.attachments = attachments;
  }
  ws.send("send_message", payload);
}
