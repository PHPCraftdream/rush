// Native-Node tests for the notices store (update banner + agent error).
import assert from "node:assert/strict";
import { test } from "node:test";
import { flush } from "@react-carburetor/core/testing";
import { NoticesCarburetor } from "./state/NoticesCarburetor.ts";
import type { INoticesState } from "./state/NoticesCarburetor.ts";

const fresh = (): NoticesCarburetor => new NoticesCarburetor({ updateAvailable: null, agentError: null });
const selectAgentError = (state: INoticesState): string | null => state.agentError;

test("setAgentError shows and clearAgentError dismisses without touching the update notice", () => {
  const store = fresh();
  store.setUpdateAvailable({ current: "1.0.0", latest: "1.1.0" });
  store.setAgentError("boom");
  assert.equal(store.getData().agentError, "boom");
  store.clearAgentError();
  assert.equal(store.getData().agentError, null);
  assert.deepEqual({ ...store.getData().updateAvailable }, { current: "1.0.0", latest: "1.1.0" });
});

test("setUpdateAvailable stores the version pair and null removes it", () => {
  const store = fresh();
  assert.equal(store.getData().updateAvailable, null);
  store.setUpdateAvailable({ current: "1.0.0", latest: "2.0.0" });
  assert.equal(store.getData().updateAvailable?.latest, "2.0.0");
  store.setUpdateAvailable(null);
  assert.equal(store.getData().updateAvailable, null);
});

test("agent-error watchers see changes only; a later error replaces an earlier one", () => {
  const store = fresh();
  const seen: (string | null)[] = [];
  const stop = store.watch(selectAgentError, (next) => {
    seen.push(next);
  });
  try {
    store.setUpdateAvailable({ current: "1", latest: "2" });
    flush(store);
    store.setAgentError("first");
    flush(store);
    store.setAgentError("second");
    flush(store);
    store.clearAgentError();
    flush(store);
    store.clearAgentError();
    flush(store);
    assert.deepEqual(seen, ["first", "second", null]);
  } finally {
    stop();
  }
});
