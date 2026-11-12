// Regression tests for keepAlive's visibility listener ownership.
// Run with `node --test` (Node >= 23 strips erasable TypeScript natively).
import assert from "node:assert/strict";
import { test } from "node:test";
import { KeepAlive } from "./keepAlive/KeepAlive.ts";

/**
 * Installs a fake document recording visibilitychange listeners.
 *
 * @param listeners - set receiving the live listeners
 * @param strictType - whether to assert every event type is visibilitychange
 */
const installFakeDocument = (listeners: Set<EventListenerOrEventListenerObject>, strictType: boolean): void => {
  globalThis.document = {
    addEventListener(type: string, listener: EventListenerOrEventListenerObject) {
      if (strictType) assert.equal(type, "visibilitychange");
      listeners.add(listener);
    },
    removeEventListener(type: string, listener: EventListenerOrEventListenerObject) {
      if (strictType) assert.equal(type, "visibilitychange");
      listeners.delete(listener);
    },
  } as unknown as Document;
};

test("duplicate installation cleanup cannot uninstall the active listener", () => {
  const listeners = new Set<EventListenerOrEventListenerObject>();
  const previousDocument = globalThis.document;
  installFakeDocument(listeners, true);
  const keepAlive = new KeepAlive();

  try {
    const cleanupOwner = keepAlive.installAutoResume();
    const cleanupDuplicate = keepAlive.installAutoResume();
    assert.equal(listeners.size, 1);

    cleanupDuplicate();
    assert.equal(listeners.size, 1, "duplicate cleanup leaves the owner's listener installed");

    cleanupOwner();
    assert.equal(listeners.size, 0, "owner cleanup removes its listener");
    cleanupOwner();
    assert.equal(listeners.size, 0, "cleanup is idempotent");
  } finally {
    for (const listener of listeners) document.removeEventListener("visibilitychange", listener);
    globalThis.document = previousDocument;
  }
});

test("stale cleanup cannot remove a subsequent installation", () => {
  const listeners = new Set<EventListenerOrEventListenerObject>();
  const previousDocument = globalThis.document;
  installFakeDocument(listeners, false);
  const keepAlive = new KeepAlive();

  try {
    const staleCleanup = keepAlive.installAutoResume();
    staleCleanup();
    const cleanupCurrent = keepAlive.installAutoResume();
    assert.equal(listeners.size, 1);

    staleCleanup();
    assert.equal(listeners.size, 1, "stale cleanup preserves the current listener");

    cleanupCurrent();
    assert.equal(listeners.size, 0);
  } finally {
    for (const listener of listeners) document.removeEventListener("visibilitychange", listener);
    globalThis.document = previousDocument;
  }
});

test("uninstall removes the listener and a fresh instance starts without one", () => {
  const listeners = new Set<EventListenerOrEventListenerObject>();
  const previousDocument = globalThis.document;
  installFakeDocument(listeners, false);

  try {
    const keepAlive = new KeepAlive();
    keepAlive.installAutoResume();
    assert.equal(listeners.size, 1);
    keepAlive.uninstallAutoResume();
    assert.equal(listeners.size, 0);
    assert.equal(keepAlive.isRunning(), false);
  } finally {
    globalThis.document = previousDocument;
  }
});
