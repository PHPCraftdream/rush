// Unit tests for the R12-C completion index (web/src/completionIndex.ts),
// run with `node --test` (Node >= 23 strips erasable TypeScript natively).
//
// Revert-checks:
//   - "structured outcome supersedes prose..." kills a merge that lets the
//     prose path stay live authority over an existing structured outcome
//     (e.g. swapping the overwrite order in mergeCompletionIndex).
//   - "prose path still indexes historical messages..." kills dropping the
//     prose indexer from the merge (historical messages would lose badges).
//   - "two batch roots sharing one reused tool_call_id..." kills collapsing
//     the structured index onto the tool-call key alone (byRootID must stay
//     keyed by root).
//   - "structuredStatus maps every batchTerminal state..." kills a state
//     mapping regression (e.g. timed_out rendered as "error").

import assert from "node:assert/strict";
import { test } from "node:test";
import { indexAsyncJobCompletions, isAsyncCompletionNotice } from "./asyncJobCompletion.ts";
import {
  buildMyPrompts,
  mergeCompletionIndex,
  structuredStatus,
} from "./completionIndex.ts";
import type { BatchWorkCompletion, Message } from "./types.ts";

function makeMessage(overrides: Partial<Message> & { ID: string; Parts: Message["Parts"] }): Message {
  return {
    SessionID: "s1",
    Role: "user",
    Model: "",
    Provider: "",
    ReasoningEffort: "",
    CreatedAt: 1,
    UpdatedAt: 1,
    IsSummaryMessage: false,
    Pinned: false,
    Hidden: false,
    AutoResumed: false,
    BackgroundJobNotice: false,
    ...overrides,
  };
}

// toolCall + result metadata pair tying job_id "root-1" to tool call "tc1".
const historicalMessages: Message[] = [
  makeMessage({ ID: "m-user", Parts: [{ type: "text", Text: "go" }] }),
  makeMessage({
    ID: "m-calls",
    Role: "assistant",
    Parts: [{ type: "tool_call", ID: "tc1", Name: "bash", Input: "{}", Finished: true }],
  }),
  makeMessage({
    ID: "m-results",
    Role: "tool",
    Parts: [
      {
        type: "tool_result",
        ToolCallID: "tc1",
        Name: "bash",
        Content: "Async job started",
        IsError: false,
        Metadata: '{"async":true,"job_id":"root-1","status":"running"}',
      },
    ],
  }),
  makeMessage({
    ID: "m-notice",
    BackgroundJobNotice: true,
    Parts: [{ type: "text", Text: "Async job root-1 (bash) finished.\n\nprose output" }],
  }),
];

function completion(partial: Partial<BatchWorkCompletion>): BatchWorkCompletion {
  return { rootID: "root-1", nodeID: "root-1", outcomeCode: "ok", outcomeRef: "", ...partial };
}

test("structured outcome supersedes prose for the joined tool call", () => {
  const prose = indexAsyncJobCompletions(historicalMessages);
  const merged = mergeCompletionIndex(prose, [
    completion({ state: "completed", outcomeSummary: "3 tasks finished" }),
    // Wire-carried identity with NO prose metadata: the join must not need
    // the prose indexer's job_id map for this root (kills a revert of the
    // comp.toolCallID ?? fallback in mergeCompletionIndex).
    completion({ rootID: "root-9", nodeID: "root-9", toolCallID: "tc9", state: "completed", outcomeSummary: "wire-only" }),
  ]);
  const rec = merged.byToolCallID.get("tc1");
  assert.ok(rec, "structured completion joined onto tool call tc1");
  assert.equal(rec.status, "finished");
  assert.equal(rec.content, "3 tasks finished");
  assert.equal(rec.source, "structured");
  assert.equal(rec.rootID, "root-1");
  assert.ok(merged.byRootID.get("root-1"));
  const rec9 = merged.byToolCallID.get("tc9");
  assert.ok(rec9, "a structured completion joins via its own wire toolCallID");
  assert.equal(rec9.source, "structured");
  assert.equal(rec9.content, "wire-only");
});

test("prose path still indexes historical messages with no structured outcome", () => {
  const prose = indexAsyncJobCompletions(historicalMessages);
  const merged = mergeCompletionIndex(prose, []);
  const rec = merged.byToolCallID.get("tc1");
  assert.ok(rec, "historical prose badge survives an empty structured set");
  assert.equal(rec.status, "finished");
  assert.equal(rec.content, "prose output");
  assert.equal(rec.source, "prose");
  assert.ok(merged.attachedNoticeIDs.has("m-notice"));
  assert.ok(merged.byRootID.size === 0);
});

test("two batch roots sharing one reused tool_call_id stay distinct by rootID", () => {
  const prose = indexAsyncJobCompletions(historicalMessages);
  const merged = mergeCompletionIndex(prose, [
    completion({ rootID: "root-1", nodeID: "root-1", toolCallID: "tc1", state: "completed", outcomeSummary: "first" }),
    completion({ rootID: "root-2", nodeID: "root-2", toolCallID: "tc1", state: "failed", outcomeSummary: "second" }),
  ]);
  // Distinctness is delivered per root; byToolCallID intentionally holds
  // the latest record for the single shared tool-call row.
  assert.equal(merged.byRootID.size, 2);
  assert.equal(merged.byRootID.get("root-1")?.status, "finished");
  assert.equal(merged.byRootID.get("root-2")?.status, "failed");
});

test("structuredStatus maps every batchTerminal state", () => {
  assert.equal(structuredStatus("completed", ""), "finished");
  assert.equal(structuredStatus("failed", ""), "failed");
  assert.equal(structuredStatus("timed_out", ""), "timed_out");
  assert.equal(structuredStatus("cancelled", ""), "cancelled");
  assert.equal(structuredStatus("interrupted", ""), "cancelled");
  assert.equal(structuredStatus("skipped", ""), "cancelled");
  // Node-level completions carry only an outcome code.
  assert.equal(structuredStatus("", "ok"), "finished");
  assert.equal(structuredStatus("", "boom"), "failed");
});

test("buildMyPrompts keeps HumanTyped rows and drops completion notices", () => {
  const typed: Message = {
    ...makeMessage({ ID: "m-typed", Parts: [{ type: "text", Text: "hello" }] }),
    HumanTyped: true,
  };
  const notice: Message = {
    ...makeMessage({
      ID: "m-notice2",
      BackgroundJobNotice: true,
      Parts: [{ type: "text", Text: "Async job root-1 (bash) finished.\n\nout" }],
    }),
    HumanTyped: true,
  };
  const prompts = buildMyPrompts([typed, notice], isAsyncCompletionNotice);
  assert.deepEqual(
    prompts.map((p) => p.id),
    ["m-typed"],
  );
});
