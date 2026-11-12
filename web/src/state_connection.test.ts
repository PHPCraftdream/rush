// Native-Node tests for the connection store (stores are plain classes: no renderer needed).
import assert from "node:assert/strict";
import { test } from "node:test";
import { flush } from "@react-carburetor/core/testing";
import { ConnectionCarburetor } from "./state/ConnectionCarburetor.ts";
import type { IConnectionState } from "./state/ConnectionCarburetor.ts";

const selectConnected = (state: IConnectionState): boolean => state.connected;
const selectShuttingDown = (state: IConnectionState): boolean => state.serverShuttingDown;

test("starts disconnected, not shutting down; setConnected flips only the link flag", () => {
  const store = new ConnectionCarburetor({ connected: false, serverShuttingDown: false, outboxCount: 0 });
  assert.equal(store.getData().connected, false);
  assert.equal(store.getData().serverShuttingDown, false);
  store.setConnected(true);
  assert.equal(store.getData().connected, true);
  assert.equal(store.getData().serverShuttingDown, false);
  store.setConnected(false);
  assert.equal(store.getData().connected, false);
});

test("markServerShuttingDown is terminal and independent of the link flag", () => {
  const store = new ConnectionCarburetor({ connected: true, serverShuttingDown: false, outboxCount: 0 });
  store.markServerShuttingDown();
  assert.equal(store.getData().serverShuttingDown, true);
  assert.equal(store.getData().connected, true);
  store.setConnected(false);
  store.setConnected(true);
  assert.equal(store.getData().serverShuttingDown, true);
});

test("watchers see each real change of their own field and ignore same-value writes", () => {
  const store = new ConnectionCarburetor({ connected: false, serverShuttingDown: false, outboxCount: 0 });
  const link: boolean[] = [];
  const down: boolean[] = [];
  const stopLink = store.watch(selectConnected, (next) => {
    link.push(next);
  });
  const stopDown = store.watch(selectShuttingDown, (next) => {
    down.push(next);
  });
  try {
    store.setConnected(true);
    flush(store);
    store.setConnected(true);
    flush(store);
    store.markServerShuttingDown();
    flush(store);
    store.markServerShuttingDown();
    flush(store);
    store.setConnected(false);
    flush(store);
    assert.deepEqual(link, [true, false]);
    assert.deepEqual(down, [true]);
  } finally {
    stopLink();
    stopDown();
  }
});
