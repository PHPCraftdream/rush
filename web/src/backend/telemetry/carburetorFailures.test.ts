// Native-Node tests: an isolated engine failure reaches the registered sink as a
// telemetry payload, and the engine itself hands the handler the ORIGINAL error
// object and a source name (delivery path).
import assert from "node:assert/strict";
import { test } from "node:test";
import { Carburetor, diagnostics } from "@react-carburetor/core/engine";
import { type CarburetorFailurePayload, formatCarburetorFailure } from "./carburetorFailureFormat.ts";
import { registerCarburetorFailureHandler } from "./carburetorFailures.ts";

// Subscribers are called synchronously by setData, so a throwing one is
// isolated (not rethrown) and reported before setData returns.
const provokeDeliveryFailure = (thrown: Error): void => {
  const store = new Carburetor({ count: 0 });
  const subscriptionId = store.subscribe(() => {
    throw thrown;
  });
  try {
    store.setData({ count: 1 });
  } finally {
    store.unsubscribe(subscriptionId);
  }
};

test("an isolated subscriber failure reaches the report sink as a telemetry payload", () => {
  const received: CarburetorFailurePayload[] = [];
  registerCarburetorFailureHandler((payload) => {
    received.push(payload);
  });
  try {
    const thrown = new Error("subscriber boom");
    provokeDeliveryFailure(thrown);
    assert.equal(received.length, 1);
    assert.deepEqual(received[0], {
      message: "subscriber boom",
      source: "carburetor:delivery",
      stack: thrown.stack,
    });
  } finally {
    diagnostics.setFailureHandler(undefined);
  }
});

test("the engine passes the original error object and the source to the failure handler", () => {
  const received: Array<{ error: unknown; source: string }> = [];
  diagnostics.setFailureHandler((error, source) => {
    received.push({ error, source });
  });
  try {
    const thrown = new Error("subscriber boom");
    provokeDeliveryFailure(thrown);
    assert.equal(received.length, 1);
    assert.equal(received[0].error, thrown);
    assert.equal(received[0].source, "delivery");
  } finally {
    diagnostics.setFailureHandler(undefined);
  }
});

test("a non-Error failure is stringified and keeps its source", () => {
  assert.deepEqual(formatCarburetorFailure("plain text", "computed"), {
    message: "plain text",
    source: "carburetor:computed",
    stack: undefined,
  });
});
