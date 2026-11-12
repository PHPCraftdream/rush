// Native-Node tests for the messages store: upsert/merge, snapshots racing live pushes, sub-agent state.
import assert from "node:assert/strict";
import { test } from "node:test";
import { MessagesCarburetor } from "./state/MessagesCarburetor.ts";
import type { Message } from "./types.ts";

/**
 * Builds a message.
 *
 * @param ID - message id
 * @param patch - fields to override
 */
const msg = (ID: string, patch: Record<string, unknown> = {}): Message =>
  ({ ID, SessionID: "s1", Role: "user", Parts: [], CreatedAt: 1, UpdatedAt: 1, ...patch }) as unknown as Message;

/**
 * Merge strategy that always prefers the incoming row.
 *
 * @param _existing - the live row
 * @param incoming - the pushed row
 */
const takeIncoming = (_existing: Message, incoming: Message): Message => incoming;

/**
 * Message id of a row.
 *
 * @param m - the message
 */
const idOf = (m: Message): string => m.ID;

/**
 * A fresh, empty store.
 */
const fresh = (): MessagesCarburetor =>
  new MessagesCarburetor(
    { messages: [], subAgentSessions: {}, subAgentMessages: {}, messageBlockBreaks: {} },
    takeIncoming,
  );

test("upsertMessage appends, then replaces by id; removeMessage drops one", () => {
  const store = fresh();
  store.upsertMessage(msg("a"));
  store.upsertMessage(msg("b"));
  store.upsertMessage(msg("a", { UpdatedAt: 5 }));
  assert.deepEqual(store.getData().messages.map(idOf), ["a", "b"]);
  assert.equal(store.getData().messages[0].UpdatedAt, 5);
  store.removeMessage("a");
  assert.deepEqual(store.getData().messages.map(idOf), ["b"]);
});

test("a raced snapshot merges: tombstoned rows stay out, newer rows win, live-only rows are kept", () => {
  const store = fresh();
  store.setMessages([msg("keep", { UpdatedAt: 9 }), msg("live-only", { UpdatedAt: 3 })]);
  store.tombstone("s1", "gone");
  store.applyMessagesSnapshot("s1", [msg("keep", { UpdatedAt: 4 }), msg("gone"), msg("fresh")], true);
  assert.deepEqual(store.getData().messages.map(idOf), ["keep", "fresh", "live-only"]);
  assert.equal(store.getData().messages[0].UpdatedAt, 9, "the fresher live row wins for non-assistant roles");
});

test("a clean snapshot replaces wholesale and forgets tombstones", () => {
  const store = fresh();
  store.setMessages([msg("old")]);
  store.tombstone("s1", "x");
  store.applyMessagesSnapshot("s1", [msg("new")], false);
  assert.deepEqual(store.getData().messages.map(idOf), ["new"]);
  store.applyMessagesSnapshot("s1", [msg("x")], true);
  assert.deepEqual(store.getData().messages.map(idOf), ["x", "new"]);
});

test("sub-agent transcripts are registered, edited, merged and reaped with their parent", () => {
  const store = fresh();
  store.registerSubAgentSession("sub", "parent");
  assert.equal(store.isSubAgentSession("sub"), true);
  assert.equal(store.getParentSessionID("sub"), "parent");
  assert.equal(store.isSubAgentSession("constructor"), false);
  store.upsertSubAgentMessage("sub", msg("m1", { SessionID: "sub" }));
  store.upsertSubAgentMessage("sub", msg("m2", { SessionID: "sub" }));
  store.removeSubAgentMessage("sub", "m1");
  assert.deepEqual(store.getData().subAgentMessages["sub"].map(idOf), ["m2"]);
  assert.deepEqual([...store.parentSessionIDs()], ["parent"]);
  store.removeSubAgentState("parent", false);
  assert.equal(store.isSubAgentSession("sub"), false);
  assert.deepEqual(store.getData().subAgentMessages, {});
});

test("trackMessageParts marks a block break only after a gap and never for tool results", () => {
  const store = fresh();
  const realNow = Date.now;
  let clock = 1_000_000;
  Date.now = () => clock;
  try {
    store.trackMessageParts("m", [{ type: "text" }]);
    clock += 6000;
    store.trackMessageParts("m", [{ type: "text" }, { type: "tool_call" }]);
    assert.deepEqual(store.getData().messageBlockBreaks["m"], [1]);
    clock += 6000;
    store.trackMessageParts("m", [{ type: "text" }, { type: "tool_call" }, { type: "tool_result" }]);
    assert.deepEqual(store.getData().messageBlockBreaks["m"], [1]);
  } finally {
    Date.now = realNow;
  }
});
