import type { Message } from "../types";
import type { AsyncJobCompletion, AsyncJobCompletionIndex, AsyncJobStatus } from "./asyncJobTypes";
import { ASYNC_NOTICE_PATTERNS } from "./ASYNC_NOTICE_PATTERNS.ts";
import { messageText } from "./messageText.ts";
import { resultMetadata } from "./resultMetadata.ts";
import { stoppedStatus } from "./stoppedStatus.ts";

/** Job-to-tool-call tables filled while scanning a transcript. */
interface IJobTables {
  asyncCalls: Map<string, string>;
  shellCalls: Map<string, string>;
  toolCallIDByJobID: Map<string, string>;
}

/**
 * Records which tool call started each async job or background shell.
 *
 * @param message - the message to scan
 * @param tables - tables to fill
 */
const recordJobStarts = (message: Message, tables: IJobTables): void => {
  for (const part of message.Parts) {
    if (part.type !== "tool_result") continue;
    const metadata = resultMetadata(part);
    if (metadata?.async && metadata.job_id) {
      tables.asyncCalls.set(metadata.job_id, part.ToolCallID);
      tables.toolCallIDByJobID.set(metadata.job_id, part.ToolCallID);
    }
    if (metadata?.background && metadata.shell_id) {
      tables.shellCalls.set(metadata.shell_id, part.ToolCallID);
      tables.toolCallIDByJobID.set(metadata.shell_id, part.ToolCallID);
    }
  }
};

/**
 * Maps a finished/failed notice onto a status.
 *
 * A user-stopped delegation (stop_agent) reaches the parent as a plain FAILED
 * completion whose entire content is subAgentOutcomeCancelledText
 * (internal/agent/work_ledger_delegation.go): the child did not fail, the
 * parent stopped it. That exact shape maps to "stopped" so the block shows
 * "stopped by user", not "error". Both delegation tool names
 * (agent/agentic_fetch) share this delivery path.
 *
 * @param outcome - "finished" or "failed"
 * @param content - the notice body
 */
const finishedStatus = (outcome: string, content: string): AsyncJobStatus =>
  outcome === "failed" && content.trim() === "sub-agent canceled" ? "stopped" : (outcome as AsyncJobStatus);

/**
 * Indexes the completion notices of a transcript by the tool call that started each job.
 *
 * @param messages - the transcript
 */
export function indexAsyncJobCompletions(messages: readonly Message[]): AsyncJobCompletionIndex {
  const byToolCallID = new Map<string, AsyncJobCompletion>();
  const attachedNoticeIDs = new Set<string>();
  const tables: IJobTables = { asyncCalls: new Map(), shellCalls: new Map(), toolCallIDByJobID: new Map() };

  for (const message of messages) {
    recordJobStarts(message, tables);

    if (message.Role !== "user") continue;
    const text = messageText(message);
    const asyncMatch = ASYNC_NOTICE_PATTERNS.finished.exec(text);
    if (asyncMatch) {
      const toolCallID = tables.asyncCalls.get(asyncMatch[1]);
      if (toolCallID) {
        byToolCallID.set(toolCallID, { status: finishedStatus(asyncMatch[2], asyncMatch[3]), content: asyncMatch[3] });
        attachedNoticeIDs.add(message.ID);
      }
      continue;
    }

    const stoppedMatch = ASYNC_NOTICE_PATTERNS.stopped.exec(text);
    if (stoppedMatch) {
      const toolCallID = tables.asyncCalls.get(stoppedMatch[1]);
      if (toolCallID) {
        byToolCallID.set(toolCallID, { status: stoppedStatus(stoppedMatch[2]), content: stoppedMatch[4] });
        attachedNoticeIDs.add(message.ID);
      }
      continue;
    }

    if (!message.BackgroundJobNotice) continue;
    const legacyMatch = ASYNC_NOTICE_PATTERNS.legacy.exec(text);
    if (!legacyMatch) continue;
    const toolCallID = tables.shellCalls.get(legacyMatch[1]);
    if (toolCallID) {
      byToolCallID.set(toolCallID, { status: Number(legacyMatch[2]) === 0 ? "finished" : "failed", content: text });
      attachedNoticeIDs.add(message.ID);
    }
  }

  return { byToolCallID, attachedNoticeIDs, toolCallIDByJobID: tables.toolCallIDByJobID };
}
