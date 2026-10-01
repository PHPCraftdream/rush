import type { Message, ToolResult } from "./types";

// Stage 5b: the terminal-cause statuses the contract's §5.1 texts carry.
// "timed_out"/"stopped"/"cancelled" arrive via FormatAsyncCompletion's
// stop/timeout/cancel wordings, matched by asyncStoppedPattern below.
export type AsyncJobStatus = "finished" | "failed" | "timed_out" | "stopped" | "cancelled";

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
// Non-finished terminal causes, each mapped to its own status so the tool-call
// block can show "timed out" / "stopped by user" / "cancelled" distinctly
// (stage 5b). Same lead-in as asyncNoticePattern by construction (both are
// FormatAsyncCompletion outputs).
const asyncStoppedPattern =
  /^Async job (\S+) \([^)]*\) (timed out after \d+s and was stopped|was stopped \(job_kill\)|was cancelled \(session stopped\))\. Partial output( before the stop)?:\n\n([\s\S]*)$/;

function stoppedStatus(cause: string): AsyncJobStatus {
  if (cause.startsWith("timed out")) return "timed_out";
  if (cause.includes("job_kill")) return "stopped";
  return "cancelled";
}
const legacyNoticePattern = /^Background job (\S+) \([^\n]*\) finished: exit (-?\d+), ran [^\n]+\./;

export function isAsyncCompletionNotice(message: Message): boolean {
  if (message.Role !== "user") return false;
  const text = messageText(message);
  return asyncNoticePattern.test(text) || asyncStoppedPattern.test(text);
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
        // A user-stopped delegation (stop_agent) reaches the parent as a
        // plain FAILED completion whose entire content is
        // subAgentOutcomeCancelledText (internal/agent/work_ledger_delegation.go)
        // — the child did not fail, the parent stopped it. Map that exact
        // shape to "stopped" so the block shows "stopped by user", not
        // "error". Both delegation tool names (agent/agentic_fetch) share
        // this delivery path.
        const status: AsyncJobStatus =
          asyncMatch[2] === "failed" && asyncMatch[3].trim() === "sub-agent canceled"
            ? "stopped"
            : (asyncMatch[2] as AsyncJobStatus);
        byToolCallID.set(toolCallID, { status, content: asyncMatch[3] });
        attachedNoticeIDs.add(message.ID);
      }
      continue;
    }

    const stoppedMatch = asyncStoppedPattern.exec(text);
    if (stoppedMatch) {
      const toolCallID = asyncCalls.get(stoppedMatch[1]);
      if (toolCallID) {
        byToolCallID.set(toolCallID, { status: stoppedStatus(stoppedMatch[2]), content: stoppedMatch[4] });
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
