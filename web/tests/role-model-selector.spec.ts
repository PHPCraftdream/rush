/**
 * Task #1061: ModelSelector generalized from smart/fast-only to all four
 * session model slots (smart/fast/worker/reviewer). Worker/reviewer live
 * behind a collapsed "toggle-role-models" button in ChatToolbar (task
 * #1060's server-side worker/reviewer plumbing already existed; this spec
 * covers the web UI on top of it).
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession, makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

const CONFIG_WITH_MODELS = makeConfig({
  providers: {
    anthropic: {
      enabled: true,
      models: [
        { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
        { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
      ],
    },
  },
});

// ── Worker/reviewer selectors are collapsed by default ──────────────────────

test("worker/reviewer selectors are hidden until the toggle is clicked", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "rm-collapsed", Title: "Collapsed Roles" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: CONFIG_WITH_MODELS });
  await expect(page.getByText("Collapsed Roles").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Collapsed Roles").first().click();

  // Smart/fast are always visible.
  await expect(page.getByTestId("model-selector-smart")).toBeVisible({ timeout: 3000 });
  await expect(page.getByTestId("model-selector-fast")).toBeVisible();
  // Worker/reviewer are not, until the toggle is used.
  await expect(page.getByTestId("model-selector-worker")).toHaveCount(0);
  await expect(page.getByTestId("model-selector-reviewer")).toHaveCount(0);

  await page.getByTestId("toggle-role-models").click();
  await expect(page.getByTestId("model-selector-worker")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("model-selector-reviewer")).toBeVisible();
});

// ── Selecting a worker/reviewer model ────────────────────────────────────────

test("selecting a worker model sends set_session_models with the worker slot only", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "rm-worker", Title: "Worker Pick" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: CONFIG_WITH_MODELS });
  await expect(page.getByText("Worker Pick").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Worker Pick").first().click();

  await page.getByTestId("toggle-role-models").click();
  await page.getByTestId("model-selector-worker").click();
  await page.getByTestId("model-dropdown").getByText("claude-haiku-4").click();

  const cmd = await waitForWSSend(page, "set_session_models");
  const p = cmd.payload as {
    sessionID: string;
    workerModel?: { provider: string; model: string };
    smartModel?: unknown;
    fastModel?: unknown;
    reviewerModel?: unknown;
  };
  expect(p.sessionID).toBe("rm-worker");
  expect(p.workerModel).toEqual({ provider: "anthropic", model: "claude-haiku-4" });
  // Only the picked slot is sent — the other three keys must be entirely
  // absent, not just falsy (task #461's nil-means-untouched convention).
  expect(p.smartModel).toBeUndefined();
  expect(p.fastModel).toBeUndefined();
  expect(p.reviewerModel).toBeUndefined();
});

test("selecting a reviewer model sends set_session_models with the reviewer slot only", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "rm-reviewer", Title: "Reviewer Pick" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: CONFIG_WITH_MODELS });
  await expect(page.getByText("Reviewer Pick").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Reviewer Pick").first().click();

  await page.getByTestId("toggle-role-models").click();
  await page.getByTestId("model-selector-reviewer").click();
  await page.getByTestId("model-dropdown").getByText("claude-opus-4").click();

  const cmd = await waitForWSSend(page, "set_session_models");
  const p = cmd.payload as { sessionID: string; reviewerModel?: { provider: string; model: string } };
  expect(p.sessionID).toBe("rm-reviewer");
  expect(p.reviewerModel).toEqual({ provider: "anthropic", model: "claude-opus-4" });
});

// ── Reset (inherit) for worker/reviewer ──────────────────────────────────────

test("resetting a worker override sends an explicit empty worker model", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({
      ID: "rm-worker-reset",
      Title: "Worker Reset",
      WorkerModelProvider: "anthropic",
      WorkerModelID: "claude-haiku-4",
    })],
  });
  await sendMockWSMessage(page, { type: "config", payload: CONFIG_WITH_MODELS });
  await expect(page.getByText("Worker Reset").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Worker Reset").first().click();

  await page.getByTestId("toggle-role-models").click();
  await page.getByTestId("model-selector-worker").click();
  await expect(page.getByTestId("model-inherit-worker")).toBeVisible({ timeout: 2000 });
  await page.getByTestId("model-inherit-worker").click();

  const cmd = await waitForWSSend(page, "set_session_models");
  const p = cmd.payload as { sessionID: string; workerModel?: { provider: string; model: string } };
  expect(p.sessionID).toBe("rm-worker-reset");
  // Explicit empty object, distinct from an omitted key — see
  // store.ts's clearSessionModelSlot doc comment.
  expect(p.workerModel).toEqual({ provider: "", model: "" });
});

test("resetting a reviewer override sends an explicit empty reviewer model", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({
      ID: "rm-reviewer-reset",
      Title: "Reviewer Reset",
      ReviewerModelProvider: "anthropic",
      ReviewerModelID: "claude-opus-4",
    })],
  });
  await sendMockWSMessage(page, { type: "config", payload: CONFIG_WITH_MODELS });
  await expect(page.getByText("Reviewer Reset").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Reviewer Reset").first().click();

  await page.getByTestId("toggle-role-models").click();
  await page.getByTestId("model-selector-reviewer").click();
  await expect(page.getByTestId("model-inherit-reviewer")).toBeVisible({ timeout: 2000 });
  await page.getByTestId("model-inherit-reviewer").click();

  const cmd = await waitForWSSend(page, "set_session_models");
  const p = cmd.payload as { sessionID: string; reviewerModel?: { provider: string; model: string } };
  expect(p.sessionID).toBe("rm-reviewer-reset");
  expect(p.reviewerModel).toEqual({ provider: "", model: "" });
});

// ── Smart/fast keep working unchanged ────────────────────────────────────────

test("smart and fast selectors still work with the role-models panel collapsed", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "rm-smart-fast", Title: "Smart Fast Still Works" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: CONFIG_WITH_MODELS });
  await expect(page.getByText("Smart Fast Still Works").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Smart Fast Still Works").first().click();

  await page.getByTestId("model-selector-smart").click();
  await page.getByTestId("model-dropdown").getByText("claude-haiku-4").click();
  const smartCmd = await waitForWSSend(page, "set_session_models");
  expect((smartCmd.payload as { smartModel: unknown }).smartModel).toEqual({
    provider: "anthropic",
    model: "claude-haiku-4",
  });

  await page.getByTestId("model-selector-fast").click();
  await page.getByTestId("model-dropdown").getByText("claude-opus-4").click();
  const fastCmd = await page.waitForFunction(
    () => {
      const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{
        type: string;
        payload: Record<string, unknown>;
      }>;
      const cmds = sent.filter((m) => m.type === "set_session_models");
      return cmds.length >= 2 ? cmds[cmds.length - 1] : null;
    },
    { timeout: 5000 }
  );
  const last = (await fastCmd.jsonValue()) as { payload: { fastModel: unknown } };
  expect(last.payload.fastModel).toEqual({ provider: "anthropic", model: "claude-opus-4" });
});
