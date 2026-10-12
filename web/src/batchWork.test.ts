// Unit tests for the batch-work store slice (web/src/batchWork.ts), run
// with `node --test` (Node >= 23 strips erasable TypeScript natively).
import assert from "node:assert/strict";
import { test } from "node:test";
import {
  $batchWorkBySession,
  applyBatchWorkSnapshot,
  pickBatchWork,
  pickMergedLiveWork,
} from "./batchWork.ts";
import type { BatchWorkSnapshot, SessionLiveWork } from "./types.ts";
import type { BatchWorkState } from "./batchWork.ts";

function snap(partial: Partial<BatchWorkSnapshot>): BatchWorkSnapshot {
  return {
    v: 1,
    sessionID: "s1",
    complete: true,
    roots: [],
    completions: [],
    ...partial,
  };
}

function root(rootID: string, toolCallID: string, state = "running") {
  return {
    rootID,
    ownerSessionID: "s1",
    toolCallID,
    kind: "task",
    taskKind: "run",
    state,
    cause: "",
    startedAt: 1,
    host: "alive",
    complete: true,
    counts: {
      pending: 0,
      starting: 0,
      running: 1,
      cancelling: 0,
      completed: 0,
      failed: 0,
      cancelled: 0,
      timed_out: 0,
      interrupted: 0,
      skipped: 0,
      active: 1,
    },
    delivery: { selected: 0, pending: 0, delivered: 0 },
    nodes: [
      {
        nodeID: rootID,
        parentID: "",
        kind: "task",
        taskKind: "run",
        toolCallID,
        state,
        cause: "",
        ordinal: 0,
        depth: 0,
        childSessionID: "",
        host: "alive",
        outcomeCode: "",
        outcomeSummary: "",
        outcomeRef: "",
      },
    ],
  };
}

test("complete snapshot replaces the last good one", () => {
  applyBatchWorkSnapshot(snap({ roots: [root("r1", "tc1")] }));
  applyBatchWorkSnapshot(snap({ roots: [root("r2", "tc2")] }));
  const state = pickBatchWork($batchWorkBySession.get(), "s1");
  assert.deepEqual(
    state.lastGood.roots.map((r) => r.rootID),
    ["r2"],
  );
});

test("incomplete snapshot merges by rootID and never drops live roots", () => {
  applyBatchWorkSnapshot(snap({ roots: [root("r1", "tc1")] }));
  applyBatchWorkSnapshot(snap({ complete: false, roots: [root("r2", "tc2")] }));
  const state = pickBatchWork($batchWorkBySession.get(), "s1");
  assert.equal(state.lastGood.complete, false);
  assert.deepEqual(
    [...state.lastGood.roots.map((r) => r.rootID)].sort(),
    ["r1", "r2"],
  );
  // A later COMPLETE snapshot still replaces.
  applyBatchWorkSnapshot(snap({ complete: true, roots: [root("r3", "tc3")] }));
  assert.deepEqual(
    pickBatchWork($batchWorkBySession.get(), "s1").lastGood.roots.map((r) => r.rootID),
    ["r3"],
  );
});

test("dedupe hides flat rows whose toolCallID a snapshot node covers", () => {
  const live = new Map<string, SessionLiveWork>([
    [
      "s1",
      {
        sessionID: "s1",
        commands: [
          { toolCallID: "tc1", toolName: "bash", title: "flat cmd", startedAt: 1 },
          { toolCallID: "tc9", toolName: "bash", title: "plain cmd", startedAt: 1 },
        ],
        agents: [
          { toolCallID: "tc2", toolName: "agent", title: "flat agent", startedAt: 1 },
        ],
      },
    ],
  ]);
  const batch = new Map<string, BatchWorkState>();
  // Flag off: no snapshot state -> flat rows unchanged.
  const flatOnly = pickMergedLiveWork(live, batch, "s1");
  assert.deepEqual(
    flatOnly.commands.map((r) => r.toolCallID),
    ["tc1", "tc9"],
  );
  applyBatchWorkSnapshot(snap({ roots: [root("r1", "tc1"), root("r2", "tc2")] }));
  const merged = pickMergedLiveWork(live, $batchWorkBySession.get(), "s1");
  assert.deepEqual(
    merged.commands.map((r) => r.toolCallID),
    ["tc9"],
  );
  assert.deepEqual(merged.agents, []);
  // Empty toolCallIDs never dedupe.
  const withEmpty = {
    ...live.get("s1"),
    commands: [{ toolCallID: "", toolName: "bash", title: "anon", startedAt: 1 }],
  };
  const live2 = new Map<string, SessionLiveWork>([["s1", withEmpty]]);
  const merged2 = pickMergedLiveWork(live2, $batchWorkBySession.get(), "s1");
  assert.equal(merged2.commands.length, 1);
});

test("unknown session picks nothing and merged view falls back to flat rows", () => {
  assert.equal(pickBatchWork($batchWorkBySession.get(), null), null);
  assert.equal(pickBatchWork($batchWorkBySession.get(), "nope"), null);
  const live = new Map<string, SessionLiveWork>([["s2", { sessionID: "s2", commands: [], agents: [] }]]);
  const merged = pickMergedLiveWork(live, $batchWorkBySession.get(), "s2");
  assert.deepEqual(merged, { commands: [], agents: [] });
});
