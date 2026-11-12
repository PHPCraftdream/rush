// Native-Node tests for the auth store (stores are plain classes: no renderer needed).
import assert from "node:assert/strict";
import { test } from "node:test";
import { flush } from "@react-carburetor/core/testing";
import { AuthCarburetor } from "./state/AuthCarburetor.ts";

const selectAuthed = (state: { authed: boolean }): boolean => state.authed;

test("starts unauthenticated and setAuthed flips the flag", () => {
  const store = new AuthCarburetor({ authed: false });
  assert.equal(store.getData().authed, false);
  store.setAuthed(true);
  assert.equal(store.getData().authed, true);
  store.setAuthed(false);
  assert.equal(store.getData().authed, false);
});

test("watchers of authed see each real change and ignore same-value writes", () => {
  const store = new AuthCarburetor({ authed: false });
  const seen: boolean[] = [];
  const stop = store.watch(selectAuthed, (next) => {
    seen.push(next);
  });
  try {
    store.setAuthed(true);
    flush(store);
    store.setAuthed(true);
    flush(store);
    store.setAuthed(false);
    flush(store);
    assert.deepEqual(seen, [true, false]);
  } finally {
    stop();
  }
});
