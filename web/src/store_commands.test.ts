// Native-Node regression tests for the production queue seams.
import assert from "node:assert/strict";
import { test } from "node:test";
import { drainSendableQueue } from "./queueOnIdle/drainSendableQueue.ts";
import { flushQueueOnIdle } from "./queueOnIdle/flushQueueOnIdle.ts";
import { sendQueuedItem } from "./queueOnIdle/sendQueuedItem.ts";
import { QueueCarburetor } from "./state/QueueCarburetor.ts";

/** One queued item as these tests model it. */
interface ITestItem {
  id: string;
  content: string;
  attachments: unknown[];
  error?: string;
  failureState?: string;
}

/**
 * Builds a queued item that has not failed.
 *
 * @param id - item id
 * @param content - item text
 */
const clean = (id: string, content: string): ITestItem => ({
  id,
  content,
  attachments: [],
  error: undefined,
  failureState: undefined,
});

/** A request that rejects like an ambiguous wsRequest failure. */
const rejectAmbiguous = async (): Promise<never> => {
  throw { ambiguous: true };
};

/** Classifies any failure as ambiguous. */
const describeAmbiguous = (): string => "ambiguous";

/** A send that must never be reached. */
const failIdleRestore = (): void => assert.fail("successful idle send must not restore");

/**
 * Builds a draining dequeue over a queue array, returning the joined sendable items.
 *
 * @param queue - the queue, drained in place
 */
const dequeueJoined =
  (queue: ITestItem[]) =>
  (): { content: string; attachments: unknown[] } | undefined => {
    const { sendable, retained } = drainSendableQueue(queue);
    queue.splice(0, queue.length, ...retained);
    return sendable.length ? { content: sendable.map(contentOf).join("\n\n"), attachments: [] } : undefined;
  };

/**
 * Text of a test item.
 *
 * @param item - the item
 */
const contentOf = (item: ITestItem): string => item.content;

/** A drained payload that is sent as-is. */
const drainedPayload = (): { content: string; attachments: unknown[] } => ({
  content: "queued",
  attachments: [{ name: "a" }],
});

/** A transport that always fails. */
const sendFails = (): boolean => false;

/**
 * Failure state of a queued message.
 *
 * @param m - the queued message
 */
const failureOf = (m: { failureState?: string }): string | undefined => m.failureState;

/**
 * Whether a queued message is the one restored as x.
 *
 * @param m - the queued message
 */
const isItemX = (m: { id: string }): boolean => m.id === "x";

/**
 * Id of a test item.
 *
 * @param item - the item
 */
const idOf = (item: ITestItem): string => item.id;

test("ambiguous per-item request restores before busy=false and is never sent as send_message", async () => {
  const events: string[] = [];
  const queued = [clean("q1", "payload")];
  let busy = true;
  await sendQueuedItem("s3", "q1", {
    take: (_sessionID, id) => {
      events.push("take");
      const index = queued.findIndex((item) => item.id === id);
      if (index < 0) return undefined;
      const [item] = queued.splice(index, 1);
      return { item, index };
    },
    request: async () => {
      events.push("wsRequest");
      return rejectAmbiguous();
    },
    describeFailure: describeAmbiguous,
    restore: (_sessionID, item, index, error) => {
      events.push("restore");
      queued.splice(index, 0, { ...item, error, failureState: "ambiguous" });
    },
  });

  busy = false;
  events.push("busy=false");
  flushQueueOnIdle("s3", {
    dequeue: () => {
      const { sendable, retained } = drainSendableQueue(queued);
      queued.splice(0, queued.length, ...retained);
      return sendable.length ? { content: sendable[0].content, attachments: [] } : undefined;
    },
    send: () => {
      events.push("send_message");
      return true;
    },
    restore: failIdleRestore,
  });

  assert.equal(busy, false);
  assert.deepEqual(events, ["take", "wsRequest", "restore", "busy=false"]);
  assert.equal(queued[0].failureState, "ambiguous");
});

test("busy=false sends only clean queue items and retains rejected items", () => {
  const queue: ITestItem[] = [
    clean("before", "earlier"),
    { ...clean("rejected", "must not retry"), failureState: "rejected" },
    clean("after", "later"),
  ];
  const sent: unknown[] = [];
  flushQueueOnIdle("s1", {
    dequeue: dequeueJoined(queue),
    send: (payload) => {
      sent.push(payload);
      return true;
    },
    restore: failIdleRestore,
  });
  assert.deepEqual(sent, [{ sessionID: "s1", content: "earlier\n\nlater" }]);
  assert.deepEqual(queue.map(idOf), ["rejected"]);
});

test("busy=false restores drained queue content when injected send fails", () => {
  let restored: unknown;
  flushQueueOnIdle("s2", {
    dequeue: drainedPayload,
    send: sendFails,
    restore: (...args) => {
      restored = args;
    },
  });
  assert.deepEqual(restored, ["s2", "queued", [{ name: "a" }]]);
});

test("store queue: dequeueAll drains clean items, keeps failed ones, joins text and attachments", () => {
  const store = new QueueCarburetor({ messageQueue: {} });
  const file = { fileName: "f", mimeType: "text/plain", data: "eA==" };
  store.enqueue("s1", "one", [file]);
  store.enqueue("s1", "two");
  const first = store.take("s1", store.getData().messageQueue["s1"][0].id)!;
  store.restoreItem("s1", first.item, first.index, "no", "rejected");
  const flushed = store.dequeueAll("s1");
  assert.deepEqual(flushed, { content: "two", attachments: [] });
  assert.deepEqual(store.getData().messageQueue["s1"].map(failureOf), ["rejected"]);
  assert.deepEqual(store.getData().messageQueue["s1"].map(contentOf), ["one"]);
  assert.equal(store.dequeueAll("missing"), undefined);
});

test("store queue: take removes with its index and restoreItem clamps to the current length", () => {
  const store = new QueueCarburetor({ messageQueue: {} });
  store.enqueue("s1", "a");
  store.enqueue("s1", "b");
  const ids = store.getData().messageQueue["s1"].map(idOf);
  const taken = store.take("s1", ids[1]);
  assert.equal(taken?.index, 1);
  assert.equal(store.take("s1", ids[1]), undefined);
  store.remove("s1", ids[0]);
  assert.equal(store.hasQueued("s1"), false);
  store.restoreItem("s1", taken!.item, 5, "failed", "ambiguous");
  const restored = store.getData().messageQueue["s1"][0];
  assert.equal(restored.error, "failed");
  assert.equal(restored.failureState, "ambiguous");
});

test("store queue: updateContent clears the error but keeps failureState; removeSession reports presence", () => {
  const store = new QueueCarburetor({ messageQueue: {} });
  store.enqueue("s1", "draft");
  const item = store.getData().messageQueue["s1"][0];
  store.restoreItem("s1", { ...item, id: "x" }, 0, "boom", "rejected");
  store.updateContent("s1", "x", "edited");
  const edited = store.getData().messageQueue["s1"].find(isItemX);
  assert.equal(edited?.content, "edited");
  assert.equal(edited?.error, undefined);
  assert.equal(edited?.failureState, "rejected");
  assert.equal(store.removeSession("s1"), true);
  assert.equal(store.removeSession("s1"), false);
});
