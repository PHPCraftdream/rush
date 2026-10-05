// Unit tests for awaitingAnswerInfo (web/src/liveWorkAwaiting.ts), run with
// `node --test` (Node >= 23 strips erasable TypeScript natively).
import assert from "node:assert/strict";
import { test } from "node:test";
import { awaitingAnswerInfo } from "./liveWorkAwaiting.ts";

test("awaiting item with a question yields badge data", () => {
  assert.deepEqual(
    awaitingAnswerInfo({
      toolCallID: "call_1",
      toolName: "agent",
      title: "delegate",
      startedAt: 1,
      status: "running",
      awaitingAnswer: true,
      awaitingQuestion: "Use adapter A or B?",
    }),
    { question: "Use adapter A or B?" },
  );
});

test("non-awaiting item yields null", () => {
  assert.equal(
    awaitingAnswerInfo({
      toolCallID: "call_2",
      toolName: "agent",
      title: "delegate",
      startedAt: 1,
      status: "running",
    }),
    null,
  );
});

test("awaiting flag without question text yields null", () => {
  assert.equal(
    awaitingAnswerInfo({
      toolCallID: "call_3",
      toolName: "agent",
      title: "delegate",
      startedAt: 1,
      status: "running",
      awaitingAnswer: true,
    }),
    null,
  );
});
