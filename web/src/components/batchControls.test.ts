import assert from "node:assert/strict";
import { test } from "node:test";
import { batchControlCommands, batchControlPayload, batchNodeNotice, batchNodeStatus, batchResultMessage, isStopConfirmed } from "./batchControls.ts";
import type { BatchWorkNode } from "../types.ts";

const pending = { nodeID: "n", parentID: "", kind: "task", taskKind: "run_command", toolCallID: "t", state: "cancelling", cause: "", ordinal: 0, depth: 0, childSessionID: "", host: "alive", outcomeCode: "", outcomeSummary: "", outcomeRef: "", noticeSelected: false } satisfies BatchWorkNode;

test("control verbs and addressed node retain nested session/root identity", () => {
  assert.deepEqual(batchControlCommands, { status: "batch_status", output: "batch_output", stop: "batch_stop", add: "batch_add", drain: "batch_drain" });
  assert.deepEqual(batchControlPayload({ sessionID: "child", rootID: "root", nodeID: "leaf", waitMS: 5000 }), { sessionID: "child", rootID: "root", nodeID: "leaf", waitMS: 5000 });
});

test("stop intent remains unconfirmed until a terminal snapshot state", () => {
  assert.equal(batchNodeStatus(pending), "cancelling");
  assert.equal(isStopConfirmed(pending), false);
  assert.equal(isStopConfirmed({ state: "cancelled" }), true);
});

test("notice messaging does not claim suppression for live nodes", () => {
  assert.equal(batchNodeNotice({ noticeSelected: true, noticeState: "pending", state: "running" }), "Delayed notice");
  assert.equal(batchNodeNotice({ noticeSelected: false, state: "failed" }), "Notice suppressed");
  assert.equal(batchNodeNotice({ noticeSelected: false, state: "running" }), "");
});

test("control refusal and accepted result are distinguished", () => {
  assert.equal(batchResultMessage({ sessionID: "s", action: "batch_stop", result: "not owner", ok: false }), "Refused: not owner");
  assert.equal(batchResultMessage({ sessionID: "s", action: "batch_stop", result: "accepted", ok: true }), "accepted");
});
