// Unit tests for isPendingJob (web/src/asyncJobMetadata.ts), run with
// `node --test` (Node >= 23 strips erasable TypeScript natively, no extra
// test runner in this package).
import assert from "node:assert/strict";
import { test } from "node:test";
import { isPendingJob } from "./asyncJobMetadata.ts";

test("async started metadata is pending", () => {
  assert.equal(isPendingJob('{"async":true,"job_id":"call_1","status":"running"}'), true);
});

test("inline result metadata is not pending", () => {
  assert.equal(
    isPendingJob('{"async":false,"inline":true,"job_id":"call_1","status":"completed"}'),
    false,
  );
});

test("background shell metadata is pending", () => {
  assert.equal(isPendingJob('{"background":true,"shell_id":"sh_1"}'), true);
});

test("ordinary result metadata is not pending", () => {
  assert.equal(isPendingJob('{"start_time":1,"end_time":2}'), false);
  assert.equal(isPendingJob(""), false);
  assert.equal(isPendingJob(undefined), false);
  assert.equal(isPendingJob("not json"), false);
});
