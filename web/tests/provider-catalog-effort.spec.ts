import { test, expect, type Page } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeConfig, makeSession } from "./helpers/fixtures";

const sessionID = "catalog-effort-session";

async function showSession(page: Page, provider: string, model: string, effort = "") {
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({
      ID: sessionID,
      SmartModelProvider: provider,
      SmartModelID: model,
      SmartModelReasoningEffort: effort,
    })],
  });
}

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
  await page.goto("/");
});

test("Codex switches use the account catalog's per-model ladder and default", async ({ page }) => {
  await showSession(page, "openai-codex", "codex-a");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig({
    models: { smart: { Provider: "openai-codex", Model: "codex-a" } },
    providers: { "openai-codex": {
      enabled: true, type: "openai-codex", name: "ChatGPT Codex",
      models: [
        { id: "codex-a", name: "Codex A", reasoningLevels: ["low", "high"], defaultReasoningEffort: "high" },
        { id: "codex-b", name: "Codex B", reasoningLevels: ["none", "minimal", "max"], defaultReasoningEffort: "max" },
      ],
    } },
  }) });

  const label = page.getByTestId("reasoning-effort-smart-label");
  await expect(label).toHaveText("H");
  await page.getByTestId("reasoning-effort-smart-increase").click();
  const first = await waitForWSSend(page, "set_session_models");
  expect(first.payload).toMatchObject({ smartModel: { reasoning_effort: "low" } });

  await showSession(page, "openai-codex", "codex-b");
  await expect(label).toHaveText("XX");
  await page.getByTestId("reasoning-effort-smart-increase").click();
  const second = await waitForWSSend(page, "set_session_models");
  expect(second.payload).toMatchObject({ smartModel: { reasoning_effort: "none" } });
});

test("StepFun and z.ai show documented tiers but never infer them from model names", async ({ page }) => {
  await showSession(page, "stepfun", "step-new");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig({
    models: { smart: { Provider: "stepfun", Model: "step-new" } },
    providers: {
      stepfun: { enabled: true, type: "openai-compat", models: [
        { id: "step-new", name: "Step New", reasoningLevels: ["low", "high"], defaultReasoningEffort: "high" },
        { id: "step-unknown", name: "Step Unknown" },
      ] },
      zai: { enabled: true, type: "openai-compat", models: [
        { id: "glm-5.3", name: "GLM-5.3" },
      ] },
    },
  }) });
  await expect(page.getByTestId("reasoning-effort-smart-label")).toHaveText("H");
  await showSession(page, "stepfun", "step-unknown");
  await expect(page.getByTestId("reasoning-effort-smart")).toHaveCount(0);
  await showSession(page, "zai", "glm-5.3");
  await expect(page.getByTestId("reasoning-effort-smart")).toHaveCount(0);
  await sendMockWSMessage(page, { type: "config", payload: makeConfig({
    models: { smart: { Provider: "zai", Model: "glm-5.3" } },
    providers: { zai: { enabled: true, type: "openai-compat", models: [
      { id: "glm-5.3", name: "GLM-5.3", reasoningLevels: ["low", "high", "max"] },
    ] } },
  }) });
  await expect(page.getByTestId("reasoning-effort-smart-label")).toHaveText("AUTO");
  await page.getByTestId("reasoning-effort-smart-increase").click();
  const sent = await waitForWSSend(page, "set_session_models");
  expect(sent.payload).toMatchObject({ smartModel: { reasoning_effort: "low" } });
});

test("Default-models picker sends an effort advertised for the selected Codex model", async ({ page }) => {
  await showSession(page, "openai-codex", "codex-b");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig({
    models: { smart: { Provider: "openai-codex", Model: "codex-b" } },
    providers: { "openai-codex": { enabled: true, type: "openai-codex", models: [
      { id: "codex-b", name: "Codex B", reasoningLevels: ["none", "high", "max"], defaultReasoningEffort: "max" },
    ] } },
  }) });
  await page.getByTestId("header-more-button").click();
  await page.getByTestId("header-default-models-button").click();
  await sendMockWSMessage(page, { type: "scoped_models", payload: {
    smart: { global: { provider: "openai-codex", model: "codex-b" }, workspace: null, effective: { provider: "openai-codex", model: "codex-b" }, effectiveScope: "global" },
    fast: { global: null, workspace: null, effective: null, effectiveScope: "" },
    worker: { global: null, workspace: null, effective: null, effectiveScope: "" },
    reviewer: { global: null, workspace: null, effective: null, effectiveScope: "" },
    hasWorkspace: false,
  } });
  const picker = page.getByTestId("scoped-models-system").getByTestId("scoped-effort-picker");
  await expect(picker).toHaveValue("max");
  await expect(picker.locator("option")).toHaveText(["none", "high", "max"]);
  await picker.selectOption("none");
  const sent = await waitForWSSend(page, "set_scoped_model");
  expect(sent.payload).toMatchObject({ reasoning_effort: "none" });
});
