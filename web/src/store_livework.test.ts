import assert from "node:assert/strict";
import { test } from "node:test";
import { isStaleWakeSnapshot } from "./wakeSnapshot/isStaleWakeSnapshot.ts";
import { pickPendingWake } from "./wakeSnapshot/pickPendingWake.ts";
import { WakeSchedulesCarburetor } from "./state/WakeSchedulesCarburetor.ts";
import { LiveWorkCarburetor } from "./state/LiveWorkCarburetor.ts";
import type { WakeScheduleItem } from "./types.ts";

/**
 * Builds an active wake schedule.
 *
 * @param id - schedule id
 */
const schedule = (id: string): WakeScheduleItem => ({
  id,
  kind: "wake",
  state: "active",
  message: `message ${id}`,
  nextRunAt: 100,
  createdAt: 1,
  everyMs: 0,
  occurrence: 0,
  maxRuns: 0,
  untilAt: 0,
});

/**
 * Whether an id is a get or cancel wake-schedules request.
 *
 * @param id - correlation id
 */
const isTaggedAny = (id: string): boolean => id.startsWith("wakesched-get-") || id.startsWith("wakesched-cancel-");

/**
 * Whether an id is a get wake-schedules request.
 *
 * @param id - correlation id
 */
const isTaggedGet = (id: string): boolean => id.startsWith("wakesched-get-");

/**
 * A fresh, empty wake-schedules store.
 */
const freshWake = (): WakeSchedulesCarburetor => new WakeSchedulesCarburetor({ bySession: {} });

test("pending cancellation stays open until its schedule disappears from snapshot", () => {
  const pending = schedule("cancel-me");
  assert.equal(pickPendingWake([schedule("cancel-me")], pending), pending);
  assert.equal(pickPendingWake([], pending), null);
  assert.equal(pickPendingWake([schedule("other")], pending), null);
  assert.equal(pickPendingWake([{ ...pending, state: "cancelled" }], pending), null);
});

test("wake schedule snapshots discard superseded request replies", () => {
  const oldID = "wakesched-get-old";
  const newID = "wakesched-get-new";
  const store = freshWake();
  store.trackRequest("session-1", newID);
  const latest = store.latestRequests();
  assert.equal(isStaleWakeSnapshot(latest, "session-1", oldID, isTaggedAny), true);
  assert.equal(isStaleWakeSnapshot(latest, "session-1", newID, isTaggedAny), false);
  assert.equal(isStaleWakeSnapshot(latest, "session-1", undefined, isTaggedAny), false);

  store.setSchedules("session-1", [schedule("new")]);
  if (!isStaleWakeSnapshot(latest, "session-1", newID, isTaggedAny)) store.setSchedules("session-1", [schedule("new")]);
  if (!isStaleWakeSnapshot(latest, "session-1", oldID, isTaggedAny)) store.setSchedules("session-1", [schedule("old")]);
  assert.deepEqual(store.getData().bySession["session-1"], [schedule("new")]);
});

test("wake schedule request freshness is isolated by session", () => {
  const store = freshWake();
  store.trackRequest("session-a", "wakesched-get-a");
  store.trackRequest("session-b", "wakesched-get-b");
  const latest = store.latestRequests();
  assert.equal(isStaleWakeSnapshot(latest, "session-a", "wakesched-get-a", isTaggedGet), false);
  assert.equal(isStaleWakeSnapshot(latest, "session-a", "wakesched-get-b", isTaggedGet), true);
});

test("setSchedules replaces one session without touching the others", () => {
  const store = freshWake();
  store.setSchedules("a", [schedule("a1")]);
  store.setSchedules("b", [schedule("b1")]);
  store.setSchedules("a", []);
  assert.deepEqual(store.getData().bySession["a"], []);
  assert.deepEqual(store.getData().bySession["b"], [schedule("b1")]);
});

test("live-work snapshots replace per session and an incomplete push never erases live work", () => {
  const store = new LiveWorkCarburetor({ bySession: {} });
  const item = { id: "c1" } as unknown as import("./types.ts").LiveWorkItem;
  store.applySnapshot({ sessionID: "s1", commands: [item], agents: [] } as never);
  assert.equal(store.getData().bySession["s1"]?.commands.length, 1);
  store.applySnapshot({ sessionID: "s1", complete: false, commands: [], agents: [] } as never);
  assert.equal(store.getData().bySession["s1"]?.commands.length, 1);
  store.applySnapshot({ sessionID: "s1", commands: [], agents: [] } as never);
  assert.equal(store.getData().bySession["s1"]?.commands.length, 0);
});
