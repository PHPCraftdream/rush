/**
 * R12-C structured batch completions (flag-gated e2e).
 *
 * With RUSH_BATCH_RUNTIME ON the server delivers terminal-root completions
 * STRUCTURALLY on the `session_batch_work` snapshot (BatchCompletionWire
 * with state + outcomeSummary, keyed by root_id) — no prose "Async job …"
 * notice message is written anymore. The tool-call block must badge the
 * completion from the structured record alone, and a reload (fresh
 * messages_list + snapshot re-read) must reconstruct it.
 *
 * GATING: the whole file is skipped unless RUSH_BATCH_RUNTIME is set to a
 * Go-strlib ParseBool truthy value (1/t/T/TRUE/true/True/on...) — any truthy
 * spelling the Go flag parser accepts — in the environment running
 * Playwright, so the default e2e set (which runs against the flag-OFF/prose
 * server) is untouched:
 *
 *   RUSH_BATCH_RUNTIME=1 npm run test -- batch-structured-completions
 *
 * Revert-check: reverting the structured branch of mergeCompletionIndex
 * (web/src/completionIndex.ts) — or the `state`/`outcomeSummary` fields on
 * BatchWorkCompletion (web/src/types.ts) — leaves "tc-struct" unbadged and
 * fails the first assertion below.
 */

import { expect, test } from "@playwright/test";
import { makeMessage, makeSession } from "./helpers/fixtures";
import { sendMockWSMessage, setupMockWS } from "./helpers/mock-ws";

// Accepts the Go strconv.ParseBool truthy set, not just the "ON" spelling:
// the server's config.BatchRuntimeEnabled uses ParseBool semantics.
const flagEnabled = /^(1|t|true)$/i.test(process.env.RUSH_BATCH_RUNTIME ?? "");

test.skip(!flagEnabled, "R12-C structured completions require RUSH_BATCH_RUNTIME to parse true (1/t/T/true/TRUE/True)");

const sessionID = "batch-struct";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
});

async function selectSession(page: import("@playwright/test").Page, id: string) {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [makeSession({ ID: id, Title: id })] });
  const row = page.getByTestId(`session-${id}`);
  await expect(row).toBeVisible({ timeout: 5000 });
  await row.click();
}

const batchMessages = [
  makeMessage({ ID: "user-b", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "run batch" }] }),
  makeMessage({
    ID: "calls-b", SessionID: sessionID, Role: "assistant",
    Parts: [{ type: "tool_call", ID: "tc-struct", Name: "bash", Input: '{"command":"echo hi"}', Finished: true }],
  }),
  makeMessage({
    ID: "results-b", SessionID: sessionID, Role: "tool",
    Parts: [{
      type: "tool_result", ToolCallID: "tc-struct", Name: "bash",
      Content: "Async job started", IsError: false,
      Metadata: '{"async":true,"job_id":"root-struct","status":"running"}',
    }],
  }),
];

test("terminal-root completion badges the tool call from the structured record alone", async ({ page }) => {
  await selectSession(page, sessionID);
  await sendMockWSMessage(page, { type: "messages_list", payload: { SessionID: sessionID, Messages: batchMessages } });

  // Structured snapshot: terminal root root-struct, NO prose notice message
  // anywhere in the transcript (the batch runtime writes none).
  await sendMockWSMessage(page, {
    type: "session_batch_work",
    payload: {
      v: 1,
      sessionID,
      complete: true,
      roots: [],
      completions: [{
        rootID: "root-struct",
        nodeID: "root-struct",
        outcomeCode: "ok",
        outcomeRef: "env-9",
        state: "completed",
        outcomeSummary: "3 tasks finished",
      }],
    },
  });

  const row = page.locator('[data-test-id="action-row"]').filter({ hasText: "echo hi" });
  await expect(row.getByText("done", { exact: true })).toBeVisible();
});

test("a browser reload reconstructs the structured completion", async ({ page }) => {
  await selectSession(page, sessionID);
  // Reload = fresh messages_list first, structured snapshot re-read after.
  await sendMockWSMessage(page, { type: "messages_list", payload: { SessionID: sessionID, Messages: batchMessages } });
  await sendMockWSMessage(page, {
    type: "session_batch_work",
    payload: {
      v: 1,
      sessionID,
      complete: true,
      roots: [],
      completions: [{
        rootID: "root-struct",
        nodeID: "root-struct",
        outcomeCode: "ok",
        outcomeRef: "",
        state: "completed",
        outcomeSummary: "3 tasks finished",
      }],
    },
  });
  await expect(
    page.locator('[data-test-id="action-row"]').filter({ hasText: "echo hi" }).getByText("done", { exact: true }),
  ).toBeVisible();

  // The REAL reload: setupMockWS is an addInitScript, so the mock transport
  // survives page.reload() and the app must reconstruct the completion from
  // a fresh messages_list + structured snapshot re-read alone.
  await page.reload();
  await sendMockWSMessage(page, { type: "sessions_list", payload: [makeSession({ ID: sessionID, Title: sessionID })] });
  const row = page.getByTestId(`session-${sessionID}`);
  await expect(row).toBeVisible({ timeout: 5000 });
  await row.click();
  await sendMockWSMessage(page, { type: "messages_list", payload: { SessionID: sessionID, Messages: batchMessages } });
  await sendMockWSMessage(page, {
    type: "session_batch_work",
    payload: {
      v: 1,
      sessionID,
      complete: true,
      roots: [],
      completions: [{
        rootID: "root-struct",
        nodeID: "root-struct",
        outcomeCode: "ok",
        outcomeRef: "",
        state: "completed",
        outcomeSummary: "3 tasks finished",
      }],
    },
  });
  await expect(
    page.locator('[data-test-id="action-row"]').filter({ hasText: "echo hi" }).getByText("done", { exact: true }),
  ).toBeVisible();

  // An INCOMPLETE follow-up read must not drop the completion.
  await sendMockWSMessage(page, {
    type: "session_batch_work",
    payload: { v: 1, sessionID, complete: false, roots: [], completions: [] },
  });
  await expect(
    page.locator('[data-test-id="action-row"]').filter({ hasText: "echo hi" }).getByText("done", { exact: true }),
  ).toBeVisible();
});
