// Native-Node tests for the wake-schedule cancel-error store.
import assert from "node:assert/strict";
import { test } from "node:test";
import { flush } from "@react-carburetor/core/testing";
import { WakeCancelCarburetor } from "./state/WakeCancelCarburetor.ts";

const selectError = (state: { error: string | null }): string | null => state.error;

test("setError stores the inline message and null clears it", () => {
  const store = new WakeCancelCarburetor({ error: null });
  assert.equal(store.getData().error, null);
  store.setError("Failed to cancel the schedule");
  assert.equal(store.getData().error, "Failed to cancel the schedule");
  store.setError(null);
  assert.equal(store.getData().error, null);
});

test("watchers see each distinct error and the clear, not repeated clears", () => {
  const store = new WakeCancelCarburetor({ error: null });
  const seen: (string | null)[] = [];
  const stop = store.watch(selectError, (next) => {
    seen.push(next);
  });
  try {
    store.setError(null);
    flush(store);
    store.setError("a");
    flush(store);
    store.setError("b");
    flush(store);
    store.setError(null);
    flush(store);
    assert.deepEqual(seen, ["a", "b", null]);
  } finally {
    stop();
  }
});
