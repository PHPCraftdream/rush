/**
 * CLI model end-to-end UI tests.
 *
 * Covers for every CLI model (claude, gemini, qwen, codex):
 *   1. Model appears in the provider config and can be selected from the dropdown.
 *   2. Selecting a model sends set_session_models with the correct provider/model.
 *   3. An assistant response is displayed in the chat after send_message.
 *
 * NOTE: The app auto-selects the first session when `sessions_list` is received,
 * so tests do NOT need to click on the session title — the session is already active.
 *
 * This file used to also cover permission_request/grant_permission/deny_permission
 * flows per CLI model, but commit bc3f3b3d ("fix(web): auto-approve all permissions
 * for web UI sessions, remove dialog") deleted PermissionDialog.tsx and every
 * associated WS event/handler/payload type — web sessions now auto-approve every
 * tool call server-side and never surface a permission prompt. Those tests were
 * removed here as stale (see task #567); permissions.spec.ts (deleted) and the two
 * permission tests removed from ui.spec.ts covered the same dead feature.
 */

import { test, expect, Page } from "@playwright/test";
import {
  setupMockWS,
  sendMockWSMessage,
  waitForWSSend,
} from "./helpers/mock-ws";
import { makeSession, makeMessage, makeConfig } from "./helpers/fixtures";

// ── Model catalogue ───────────────────────────────────────────────────────────

const CLI_MODELS = [
  { id: "cli-claude-sonnet",          name: "Claude Sonnet (CLI)",                provider: "local-cli" },
  { id: "cli-claude-opus",            name: "Claude Opus (CLI)",                  provider: "local-cli" },
  { id: "cli-claude-sonnet-thinking", name: "Claude Sonnet Thinking (CLI)",       provider: "local-cli" },
  { id: "cli-claude-opus-thinking",   name: "Claude Opus Thinking (CLI)",         provider: "local-cli" },
  { id: "cli-gemini-flash",           name: "Gemini 3 Flash (CLI)",               provider: "local-cli" },
  { id: "cli-gemini-pro",             name: "Gemini 3.1 Pro (CLI)",               provider: "local-cli" },
  { id: "cli-qwen",                   name: "Qwen 3.5 Plus (CLI)",                provider: "local-cli" },
  { id: "cli-codex",                  name: "Codex (gpt-5.3-codex, CLI)",         provider: "local-cli" },
  { id: "cli-codex-gpt-5-4",         name: "Codex (gpt-5.4, CLI)",               provider: "local-cli" },
  { id: "cli-codex-gpt-5-2",         name: "Codex (gpt-5.2-codex, CLI)",         provider: "local-cli" },
  { id: "cli-codex-max",              name: "Codex Max (gpt-5.1-codex-max, CLI)", provider: "local-cli" },
  { id: "cli-codex-gpt-5-2-base",    name: "Codex (gpt-5.2, CLI)",               provider: "local-cli" },
  { id: "cli-codex-mini",            name: "Codex Mini (gpt-5.1-codex-mini, CLI)", provider: "local-cli" },
] as const;

type CliModel = (typeof CLI_MODELS)[number];

// ── Config factory ────────────────────────────────────────────────────────────

function makeCliConfig(models: readonly CliModel[] = CLI_MODELS) {
  return makeConfig({
    models: {
      smart: { Provider: "local-cli", Model: models[0].id },
      fast: { Provider: "local-cli", Model: models[0].id },
    },
    providers: {
      "local-cli": {
        name: "CLI",
        // enabled: true makes models selectable; type: "cli" marks as CLI provider
        enabled: true,
        type: "cli",
        models: models.map((m) => ({
          id: m.id,
          name: m.name,
          contextWindow: 200_000,
        })),
      },
    },
  });
}

// ── Helpers ───────────────────────────────────────────────────────────────────

/**
 * Navigate to the app, inject sessions + config, and wait for the chat UI to be ready.
 *
 * The app auto-selects the first session when sessions_list arrives, so there
 * is no need to click on the session title in the sidebar.
 */
async function setup(page: Page, sessionID: string, title: string) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: title })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeCliConfig() });
  // Wait until the chat input is enabled (session active + config received)
  await expect(page.locator('[data-test-id="chat-input-textarea"]')).toBeEnabled({ timeout: 5000 });
}

/** Select a model from the "Smart" model dropdown in the header. */
async function selectSmartModel(page: Page, modelName: string) {
  await expect(page.locator('[data-test-id="model-selector-smart"]')).toBeVisible({ timeout: 3000 });
  await page.locator('[data-test-id="model-selector-smart"]').click();
  await page.locator('[data-test-id="model-dropdown"]').getByPlaceholder("Search models…").fill(modelName);
  await page.locator('[data-test-id="model-dropdown"]').getByRole("button", { name: modelName, exact: true }).click();
}

/** Select a model from the "Fast" model dropdown in the header. */
async function selectFastModel(page: Page, modelName: string) {
  await expect(page.locator('[data-test-id="model-selector-fast"]')).toBeVisible({ timeout: 3000 });
  await page.locator('[data-test-id="model-selector-fast"]').click();
  await page.locator('[data-test-id="model-dropdown"]').getByPlaceholder("Search models…").fill(modelName);
  await page.locator('[data-test-id="model-dropdown"]').getByRole("button", { name: modelName, exact: true }).click();
}

// ── Per-model: selection + response ──────────────────────────────────────────

for (const model of CLI_MODELS) {
  test.describe(`CLI model: ${model.name}`, () => {
    test.beforeEach(async ({ page }) => {
      await setupMockWS(page);
      await page.route("/auth/check", (route) =>
        route.fulfill({ status: 200, body: "OK" })
      );
    });

    test(`${model.id}: appears in config and can be selected as large model`, async ({ page }) => {
      const sessionID = `sel-large-${model.id}`;
      await setup(page, sessionID, `Session ${sessionID}`);

      await selectSmartModel(page, model.name);

      const cmd = await waitForWSSend(page, "set_session_models");
      const payload = cmd.payload as {
        smartModel: { provider: string; model: string };
      };
      expect(payload.smartModel).toEqual({
        provider: model.provider,
        model: model.id,
      });
    });

    test(`${model.id}: can be selected as small model`, async ({ page }) => {
      const sessionID = `sel-small-${model.id}`;
      await setup(page, sessionID, `Session ${sessionID}`);

      await selectFastModel(page, model.name);

      const cmd = await waitForWSSend(page, "set_session_models");
      const payload = cmd.payload as {
        fastModel: { provider: string; model: string };
      };
      expect(payload.fastModel).toEqual({
        provider: model.provider,
        model: model.id,
      });
    });

    test(`${model.id}: assistant response is displayed in chat`, async ({ page }) => {
      const sessionID = `chat-${model.id}`;
      await setup(page, sessionID, `Session ${sessionID}`);

      // Select this model
      await selectSmartModel(page, model.name);
      await waitForWSSend(page, "set_session_models");

      // Confirm model switch via session_updated
      await sendMockWSMessage(page, {
        type: "session_updated",
        payload: makeSession({
          ID: sessionID,
          SmartModelProvider: model.provider,
          SmartModelID: model.id,
        }),
      });

      // Send a message
      await page.locator('[data-test-id="chat-input-textarea"]').fill("Hello from test");
      await page.locator('[data-test-id="chat-input-send-button"]').click();

      const sent = await waitForWSSend(page, "send_message");
      expect((sent.payload as Record<string, unknown>).content).toBe("Hello from test");

      // Backend sends response
      const responseText = `Response from ${model.name}`;
      await sendMockWSMessage(page, {
        type: "message_created",
        payload: makeMessage({
          ID: `resp-${model.id}`,
          SessionID: sessionID,
          Role: "assistant",
          Parts: [{ type: "text", Text: responseText }],
          Model: model.id,
          Provider: model.provider,
        }),
      });

      await expect(page.getByText(responseText)).toBeVisible({ timeout: 3000 });
    });
  });
}
