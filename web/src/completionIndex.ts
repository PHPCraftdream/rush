import { isAsyncCompletionNotice } from "./asyncJobCompletion";
import type {
  AsyncJobCompletion,
  AsyncJobCompletionIndex,
  AsyncJobStatus,
} from "./asyncJobCompletion";
import type { BatchWorkCompletion, Message } from "./types";

// Completion index (R12-C): merges the structured batch completions
// (internal/server/handlers_batch.go's BatchCompletionWire, delivered on
// the session_batch_work snapshot with `state` + `outcomeSummary` for
// TERMINAL ROOTS) with the historical prose path
// (asyncJobCompletion.indexAsyncJobCompletions).
//
// Behaviour boundary:
//   - A message that carries no structured outcome keeps rendering through
//     the prose path exactly as before — read-only, unchanged. The prose
//     indexer and every pattern in it stay the live authority for history
//     written before the batch-runtime cutover.
//   - Once a structured outcome exists for a root, it supersedes the prose
//     entry for the joined tool call: the prose text is never re-parsed as
//     live authority on top of a structured record.
//   - Structured records are keyed by rootID, so two roots reusing one
//     tool_call_id stay distinct in `byRootID` even though the prose join
//     can only resolve one of them onto the (shared) tool call.
//
// Join: a structured completion names only its rootID. The tool-call side
// comes from the prose indexer's job_id→toolCallID metadata map — the batch
// root's ID is the async job identity the pre-cutover tool_result metadata
// already carries. A root with no such metadata row surfaces only in
// `byRootID` (no tool-call badge), never by guessing.
//
// This module is PURE (no atom reads/writes): store.ts wires the returned
// functions into nanostores computeds, exactly like the store_commands.ts
// split — the store.ts ↔ here cycle would otherwise be unsafe at
// module-initialization time (the React Compiler / init-order rules are
// documented in store_livework.ts and store.ts).

export type CompletionSource = "structured" | "prose";

export interface CompletionRecord extends AsyncJobCompletion {
  source: CompletionSource;
  // Set on structured records: the batch root the outcome belongs to.
  rootID?: string;
}

export interface MergedCompletionIndex {
  // Keyed by tool call ID — same shape consumers already read
  // (ToolActivityGroup → ActionRow's jobCompletions map).
  byToolCallID: Map<string, CompletionRecord>;
  // Keyed by batch root ID: every structured completion, including roots
  // whose tool_call_id could not be joined.
  byRootID: Map<string, CompletionRecord>;
  attachedNoticeIDs: Set<string>;
}

// structuredStatus maps a batch node state (internal/server/handlers_batch.go
// batchTerminal's set) onto the AsyncJobStatus vocabulary the badges render.
// An unknown state falls back to the outcome code ("ok" = finished), which
// is what node-level completions carry.
export function structuredStatus(state: string, outcomeCode: string): AsyncJobStatus {
  switch (state) {
    case "completed":
      return "finished";
    case "failed":
      return "failed";
    case "timed_out":
      return "timed_out";
    case "cancelled":
    case "interrupted":
    case "skipped":
      return "cancelled";
    default:
      return outcomeCode === "ok" ? "finished" : "failed";
  }
}

/** Merges the prose index (from messages) with the structured completions
 * of one session's batch-work snapshot. Prose entries win only where no
 * structured outcome exists for their tool call — the structured record
 * always replaces the prose one on a join hit. */
export function mergeCompletionIndex(
  prose: AsyncJobCompletionIndex,
  completions: BatchWorkCompletion[] | null | undefined,
): MergedCompletionIndex {
  const byToolCallID = new Map<string, CompletionRecord>();
  for (const [toolCallID, completion] of prose.byToolCallID) {
    byToolCallID.set(toolCallID, { ...completion, source: "prose" });
  }
  const byRootID = new Map<string, CompletionRecord>();
  for (const comp of completions ?? []) {
    if (!comp.rootID) continue;
    const record: CompletionRecord = {
      status: structuredStatus(comp.state ?? "", comp.outcomeCode ?? ""),
      content: comp.outcomeSummary ?? "",
      source: "structured",
      rootID: comp.rootID,
    };
    byRootID.set(comp.rootID, record);
    const toolCallID = prose.toolCallIDByJobID.get(comp.rootID);
    if (toolCallID) byToolCallID.set(toolCallID, record);
  }
  return { byToolCallID, byRootID, attachedNoticeIDs: new Set(prose.attachedNoticeIDs) };
}

// completionStatuses flattens the merged index to the per-tool-call status
// map SubAgentBlock consumes (the old $asyncJobStatuses shape).
export function completionStatuses(index: MergedCompletionIndex): Map<string, AsyncJobStatus> {
  const statuses = new Map<string, AsyncJobStatus>();
  for (const [toolCallID, completion] of index.byToolCallID) {
    statuses.set(toolCallID, completion.status);
  }
  return statuses;
}

// ── Composer prompt recall (moved verbatim from store.ts — it is the other
// consumer of the completion-notice predicate, and moving it keeps store.ts
// under the 1000-line limit) ─────────────────────────────────────────────────

export interface MyPromptItem {
  id: string;
  text: string;
}

export function partsToText(parts: Array<{ type: string; Text?: string }>): string {
  let out = "";
  for (const p of parts) {
    if (p.type === "text" && p.Text) out += p.Text;
  }
  return out;
}

// isComposerTypedMessage is THE web-side authorship filter — the single
// predicate shared by the arrow-history recall and the history dropdown,
// both derived from $myPrompts (store.ts). HumanTyped is computed
// server-side from structured fields only; isAsyncCompletionNotice stays as
// the content-pattern fallback for notice rows persisted before the
// BackgroundJobNotice column existed.
export function isComposerTypedMessage(m: Message): boolean {
  if (!m.HumanTyped) return false;
  if (isAsyncCompletionNotice(m)) return false;
  return true;
}

export function buildMyPrompts(msgs: Message[]): MyPromptItem[] {
  const out: MyPromptItem[] = [];
  for (const m of msgs) {
    if (!isComposerTypedMessage(m)) continue;
    const text = partsToText(m.Parts as unknown as Array<{ type: string; Text?: string }>).trim();
    if (!text) continue;
    out.push({ id: m.ID, text });
  }
  // Newest first — matches "press ↑ to get the previous prompt".
  return out.reverse();
}
