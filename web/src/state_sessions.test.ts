// Native-Node tests for the sessions store (stores are plain classes: no renderer needed).
import assert from "node:assert/strict";
import { test } from "node:test";
import { flush } from "@react-carburetor/core/testing";
import { SessionsCarburetor } from "./state/SessionsCarburetor.ts";
import type { ISessionsState } from "./state/SessionsCarburetor.ts";
import type { Session } from "./types.ts";

/**
 * Builds a minimal session row.
 *
 * @param ID - session id
 * @param patch - fields to override
 */
const session = (ID: string, patch: Record<string, unknown> = {}): Session => ({ ID, ...patch }) as unknown as Session;

/**
 * Session id of a row.
 *
 * @param s - the session
 */
const idOf = (s: Session): string => s.ID;

/**
 * Picks the active session id.
 *
 * @param state - the sessions state
 */
const selectActive = (state: ISessionsState): string | null => state.activeSessionID;

/**
 * A fresh, empty store.
 */
const fresh = (): SessionsCarburetor => new SessionsCarburetor({ sessions: [], activeSessionID: null });

test("upsert prepends a new session and replaces an existing row in place", () => {
  const store = fresh();
  store.upsert(session("a"));
  store.upsert(session("b"));
  assert.deepEqual(store.getData().sessions.map(idOf), ["b", "a"]);
  store.upsert(session("a", { Title: "renamed" }));
  assert.deepEqual(store.getData().sessions.map(idOf), ["b", "a"]);
  assert.equal(store.findSession("a")?.Title, "renamed");
  store.remove("b");
  assert.deepEqual(store.getData().sessions.map(idOf), ["a"]);
});

test("applySnapshot replays live deletes then upserts over the snapshot and returns the result", () => {
  const store = fresh();
  const next = store.applySnapshot([session("a"), session("b")], {
    upserts: [session("c"), session("a", { Title: "live" })],
    deletedIDs: ["b"],
  });
  assert.deepEqual(next.map(idOf), ["c", "a"]);
  assert.deepEqual(store.getData().sessions.map(idOf), ["c", "a"]);
  assert.equal(store.findSession("a")?.Title, "live");
  const plain = store.applySnapshot([session("z")], { upserts: [], deletedIDs: [] });
  assert.deepEqual(plain.map(idOf), ["z"]);
});

test("patchSession overwrites fields of a known session only", () => {
  const store = fresh();
  store.setSessions([session("a", { SmartModelID: "old" })]);
  assert.equal(store.patchSession("a", { SmartModelID: "new", Todos: [] }), true);
  assert.equal(store.patchSession("missing", { SmartModelID: "x" }), false);
  assert.equal((store.findSession("a") as unknown as Record<string, unknown>).SmartModelID, "new");
  assert.equal(store.findSession(null), undefined);
});

test("active-session watchers see changes only", () => {
  const store = fresh();
  const seen: (string | null)[] = [];
  const stop = store.watch(selectActive, (next) => {
    seen.push(next);
  });
  try {
    store.setActiveSessionID("a");
    flush(store);
    store.setActiveSessionID("a");
    flush(store);
    store.upsert(session("a"));
    flush(store);
    store.setActiveSessionID(null);
    flush(store);
    assert.deepEqual(seen, ["a", null]);
  } finally {
    stop();
  }
});
