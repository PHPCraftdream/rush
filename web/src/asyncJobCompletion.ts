import type { Message, ToolResult } from "./types";

export type AsyncJobStatus = "finished" | "failed";

export interface AsyncJobCompletion {
  status: AsyncJobStatus;
  content: string;
}

export interface AsyncJobCompletionIndex {
  byToolCallID: Map<string, AsyncJobCompletion>;
  attachedNoticeIDs: Set<string>;
}

function messageText(message: Message): string {
  return message.Parts.filter((part) => part.type === "text").map((part) => part.Text).join("\n");
}

function resultMetadata(part: ToolResult): { async?: boolean; job_id?: string; background?: boolean; shell_id?: string } | null {
  if (!part.Metadata) return null;
  try {
    return JSON.parse(part.Metadata);
  } catch {
    return null;
  }
}

const asyncNoticePattern = /^Async job (\S+) \([^)]*\) (finished|failed)\.\n\n([\s\S]*)$/;
const legacyNoticePattern = /^Background job (\S+) \([^\n]*\) finished: exit (-?\d+), ran [^\n]+\./;

export function isAsyncCompletionNotice(message: Message): boolean {
  return message.Role === "user" && asyncNoticePattern.test(messageText(message));
}

export function indexAsyncJobCompletions(messages: Message[]): AsyncJobCompletionIndex {
  const byToolCallID = new Map<string, AsyncJobCompletion>();
  const attachedNoticeIDs = new Set<string>();
  const asyncCalls = new Map<string, string>();
  const shellCalls = new Map<string, string>();

  for (const message of messages) {
    for (const part of message.Parts) {
      if (part.type !== "tool_result") continue;
      const metadata = resultMetadata(part);
      if (metadata?.async && metadata.job_id) asyncCalls.set(metadata.job_id, part.ToolCallID);
      if (metadata?.background && metadata.shell_id) shellCalls.set(metadata.shell_id, part.ToolCallID);
    }

    if (message.Role !== "user") continue;
    const text = messageText(message);
    const asyncMatch = asyncNoticePattern.exec(text);
    if (asyncMatch) {
      const toolCallID = asyncCalls.get(asyncMatch[1]);
      if (toolCallID) {
        byToolCallID.set(toolCallID, { status: asyncMatch[2] as AsyncJobStatus, content: asyncMatch[3] });
        attachedNoticeIDs.add(message.ID);
      }
      continue;
    }

    if (!message.BackgroundJobNotice) continue;
    const legacyMatch = legacyNoticePattern.exec(text);
    if (!legacyMatch) continue;
    const toolCallID = shellCalls.get(legacyMatch[1]);
    if (toolCallID) {
      byToolCallID.set(toolCallID, { status: Number(legacyMatch[2]) === 0 ? "finished" : "failed", content: text });
      attachedNoticeIDs.add(message.ID);
    }
  }

  return { byToolCallID, attachedNoticeIDs };
}
