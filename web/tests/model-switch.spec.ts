/**
 * Model-switch integration tests.
 *
 * Model selection now persists in the database via set_session_models,
 * not via per-message overrides in send_message. These tests verify:
 *  1. Selecting a model sends set_session_models with correct provider/model.
 *  2. The send_message payload does NOT contain smartModel/fastModel overrides.
 *  3. The session_updated event from the server updates the displayed model.
 *  4. Per-session independence is maintained.
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

// ── Helper ───────────────────────────────────────────────────────────────────

async function getSentMessages(page: import("@playwright/test").Page) {
  return page.evaluate(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{
      type: string;
      payload: unknown;
    }>;
    return sent.filter((m) => m.type === "send_message").map((m) => m.payload);
  });
}

// ── set_session_models command ────────────────────────────────────────────────

test("selecting large model sends set_session_models with provider and model", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sw-lg", Title: "Smart Switch" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: {
          enabled: true,
          models: [
            { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
            { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
          ],
        },
      },
    }),
  });
  await expect(page.getByText("Smart Switch").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Smart Switch").first().click();

  await expect(page.locator("button[title='Smart (strong) model']")).toBeVisible({ timeout: 3000 });
  await page.locator("button[title='Smart (strong) model']").click();
  await page.getByTestId("model-dropdown").getByText("claude-haiku-4").click();

  const cmd = await waitForWSSend(page, "set_session_models");
  const p = cmd.payload as { sessionID: string; smartModel: { provider: string; model: string }; fastModel: unknown };
  expect(p.sessionID).toBe("sw-lg");
  expect(p.smartModel).toEqual({ provider: "anthropic", model: "claude-haiku-4" });
});

test("selecting small model sends set_session_models with correct small provider/model", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sw-sm", Title: "Fast Switch" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: {
          enabled: true,
          models: [
            { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
            { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
          ],
        },
        openai: { enabled: true, models: [{ id: "gpt-4o-mini", name: "gpt-4o-mini", contextWindow: 128000 }] },
      },
    }),
  });
  await expect(page.getByText("Fast Switch").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Fast Switch").first().click();

  await expect(page.locator("button[title='Fast (cheap) model']")).toBeVisible({ timeout: 3000 });
  await page.locator("button[title='Fast (cheap) model']").click();
  await page.getByTestId("model-dropdown").getByText("gpt-4o-mini").click();

  const cmd = await waitForWSSend(page, "set_session_models");
  const p = cmd.payload as { sessionID: string; fastModel: { provider: string; model: string } };
  expect(p.sessionID).toBe("sw-sm");
  expect(p.fastModel).toEqual({ provider: "openai", model: "gpt-4o-mini" });
});

test("set_session_models includes both models", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sw-both", Title: "Both Switch" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: {
          enabled: true,
          models: [
            { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
            { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
          ],
        },
        openai: { enabled: true, models: [{ id: "gpt-4o-mini", name: "gpt-4o-mini", contextWindow: 128000 }] },
      },
    }),
  });
  await expect(page.getByText("Both Switch").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Both Switch").first().click();

  // Pick large model
  await page.locator("button[title='Smart (strong) model']").click();
  await page.getByTestId("model-dropdown").getByText("claude-haiku-4").click();
  await waitForWSSend(page, "set_session_models");

  // Pick small model — second set_session_models command
  await page.locator("button[title='Fast (cheap) model']").click();
  await page.getByTestId("model-dropdown").getByText("gpt-4o-mini").click();

  const secondCmd = await page.waitForFunction(
    () => {
      const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{
        type: string;
        payload: Record<string, unknown>;
      }>;
      const cmds = sent.filter((m) => m.type === "set_session_models");
      return cmds.length >= 2 ? cmds[cmds.length - 1] : null;
    },
    { timeout: 5_000 }
  );
  const last = await secondCmd.jsonValue() as { payload: { fastModel: { provider: string; model: string } } };
  expect(last.payload.fastModel).toEqual({ provider: "openai", model: "gpt-4o-mini" });
});

// ── send_message has no overrides ────────────────────────────────────────────

test("send_message does not include smartModel or fastModel overrides", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sw-no-ov", Title: "No Override" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("No Override").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("No Override").first().click();

  // Switch model
  await page.locator("button[title='Smart (strong) model']").click();
  await page.getByTestId("model-dropdown").getByText("claude-haiku-4").click();
  await waitForWSSend(page, "set_session_models");

  // Send a message
  await page.getByTestId("chat-input-textarea").fill("hello");
  await page.getByTestId("chat-input-send-button").click();

  const sent = await waitForWSSend(page, "send_message");
  const payload = sent.payload as Record<string, unknown>;
  expect(payload.smartModel).toBeUndefined();
  expect(payload.fastModel).toBeUndefined();
  expect(payload.content).toBe("hello");
});

test("send_message never contains model overrides even on default model", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sw-def", Title: "Default Model" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Default Model").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Default Model").first().click();

  await page.getByTestId("chat-input-textarea").fill("hello default");
  await page.getByTestId("chat-input-send-button").click();

  const sent = await waitForWSSend(page, "send_message");
  const payload = sent.payload as Record<string, unknown>;
  expect(payload.smartModel).toBeUndefined();
  expect(payload.fastModel).toBeUndefined();
  expect(payload.sessionID).toBe("sw-def");
});

// ── sendWithFastModel fallback (task #570, F6) ────────────────────────────────

test("send-with-fast-model button sends the config FAST model when the session has no explicit fast override", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    // No FastModelID/FastModelProvider set — sendWithFastModel must fall
    // back to config.models.fast.
    payload: [makeSession({ ID: "sw-fast-fallback", Title: "Fast Fallback" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      models: {
        smart: { Provider: "anthropic", Model: "claude-opus-4" },
        fast: { Provider: "anthropic", Model: "claude-haiku-4" },
      },
      providers: {
        anthropic: {
          enabled: true,
          models: [
            { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
            { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
          ],
        },
      },
    }),
  });
  await expect(page.getByText("Fast Fallback").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Fast Fallback").first().click();

  await page.getByPlaceholder("Message… (/ for skills, Enter to send)").fill("hello fast");
  await page.locator("button[title='Send with lightweight model']").click();

  const sent = await waitForWSSend(page, "send_message");
  const payload = sent.payload as { sessionID: string; content: string; smartModel?: { provider: string; model: string } };
  expect(payload.sessionID).toBe("sw-fast-fallback");
  expect(payload.content).toBe("hello fast");
  // F6 regression: sendWithFastModel's fallback used to read
  // stale-slot-ok: naming the dead key is the whole point of this test
  // `config?.models?.small`, a key the server never sends (the wire uses
  // `SelectedModelType` values "smart"/"fast" — see
  // internal/server/handlers_config.go's buildConfigWire). With the stale
  // key, `fastModel` stayed undefined and the message went out with NO
  // override at all, silently running on the SMART model instead of FAST.
  // The override rides on the `smartModel` wire field by protocol design
  // (internal/server/protocol.go's SendMessagePayload) — sendWithFastModel
  // puts the resolved fast model there so the agent actually uses it for
  // this turn.
  expect(payload.smartModel).toEqual({ provider: "anthropic", model: "claude-haiku-4" });
});

// ── session_updated reflects model change in header ──────────────────────────

test("session_updated with model fields updates header model button", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sw-upd", Title: "Update Model" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Update Model").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Update Model").first().click();

  // Server sends session_updated with model fields filled in
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({
      ID: "sw-upd",
      Title: "Update Model",
      SmartModelProvider: "anthropic",
      SmartModelID: "claude-haiku-4",
    }),
  });

  // Header large model button should now show claude-haiku-4
  await expect(
    page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-haiku-4" })
  ).toBeVisible({ timeout: 2000 });
});

// ── Per-session independence ──────────────────────────────────────────────────

test("switching model in session A does not affect session B", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "psw-1", Title: "Session A" }),
      makeSession({ ID: "psw-2", Title: "Session B" }),
    ],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: {
          enabled: true,
          models: [
            { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
            { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
          ],
        },
      },
    }),
  });

  // Change Session A model
  await expect(page.getByText("Session A").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Session A").first().click();
  await page.locator("button[title='Smart (strong) model']").click();
  await page.getByTestId("model-dropdown").getByText("claude-haiku-4").click();

  // Session A button shows haiku
  await expect(
    page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-haiku-4" })
  ).toBeVisible({ timeout: 2000 });

  // Switch to Session B — should show opus (default)
  await page.getByText("Session B").click();
  await expect(
    page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" })
  ).toBeVisible({ timeout: 2000 });
});

// ── Model override persists across sends in same session ─────────────────────

test("model override persists for subsequent messages via DB", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sw-persist", Title: "Persist Session" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: {
          enabled: true,
          models: [
            { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
            { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
          ],
        },
      },
    }),
  });
  await expect(page.getByText("Persist Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Persist Session").first().click();

  // Switch model once
  await page.locator("button[title='Smart (strong) model']").click();
  await page.getByTestId("model-dropdown").getByText("claude-haiku-4").click();
  await waitForWSSend(page, "set_session_models");

  // Simulate server confirming the session model update
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({
      ID: "sw-persist",
      Title: "Persist Session",
      SmartModelProvider: "anthropic",
      SmartModelID: "claude-haiku-4",
    }),
  });

  // Send two messages — neither should have model overrides in payload
  await page.getByTestId("chat-input-textarea").fill("msg one");
  await page.getByTestId("chat-input-send-button").click();
  await waitForWSSend(page, "send_message");

  await page.getByTestId("chat-input-textarea").fill("msg two");
  await page.getByTestId("chat-input-send-button").click();
  await page.waitForFunction(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{
      type: string;
    }>;
    return sent.filter((m) => m.type === "send_message").length >= 2;
  }, { timeout: 5000 });

  const msgs = await getSentMessages(page) as Array<Record<string, unknown>>;
  for (const msg of msgs) {
    expect(msg.smartModel).toBeUndefined();
    expect(msg.fastModel).toBeUndefined();
  }
});

// ── Inherit (task #467) ─────────────────────────────────────────────────────

test("Inherit option is only shown when the session has an explicit override", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sw-inherit-none", Title: "No Override Yet" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: {
          enabled: true,
          models: [
            { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
            { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
          ],
        },
      },
    }),
  });
  await expect(page.getByText("No Override Yet").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("No Override Yet").first().click();

  await page.locator("button[title='Smart (strong) model']").click();
  // NB: this must be getByTestId (which respects playwright.config.ts's
  // testIdAttribute: "data-test-id"), NOT a raw `[data-testid="..."]` CSS
  // selector — ModelSelector.tsx renders the hyphenated `data-test-id`
  // attribute, so a literal `data-testid` attribute selector never matches
  // anything in the real DOM and every assertion built on it is vacuous.
  await expect(page.getByTestId("model-inherit-smart")).toHaveCount(0);
});

test("selecting Inherit clears the session's override with an explicit empty model", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({
      ID: "sw-inherit",
      Title: "Has Override",
      SmartModelProvider: "anthropic",
      SmartModelID: "claude-haiku-4",
    })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: {
          enabled: true,
          models: [
            { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
            { id: "claude-haiku-4", name: "claude-haiku-4", contextWindow: 200000 },
          ],
        },
      },
    }),
  });
  await expect(page.getByText("Has Override").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Has Override").first().click();

  await page.locator("button[title='Smart (strong) model']").click();
  await expect(page.getByTestId("model-inherit-smart")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("model-inherit-smart").click();

  const cmd = await waitForWSSend(page, "set_session_models");
  const p = cmd.payload as { sessionID: string; smartModel: { provider: string; model: string } };
  expect(p.sessionID).toBe("sw-inherit");
  // Explicit empty object — distinct from an omitted/null field, which the
  // backend reads as "don't touch" rather than "clear" (see store.ts's
  // clearSessionModelSlot doc comment).
  expect(p.smartModel).toEqual({ provider: "", model: "" });

  // F5 regression (task #570): clearSessionModelSlot's local optimistic
  // stale-slot-ok: naming the dead keys is the whole point of this test
  // update indexes a `clearedFields` map keyed by the OLD "large"/"small"
  // slot names. With the stale keys, `clearedFields["smart"]` is undefined,
  // so the optimistic write lands on `undefinedProvider`/`undefinedID`
  // instead of `SmartModelProvider`/`SmartModelID`, and the header button
  // keeps showing the pre-clear model (claude-haiku-4) until a server
  // round-trip repaints it. Assert the LOCAL state directly — not just the
  // outgoing WS payload above — by checking the header button text updates
  // immediately, with no session_updated message sent.
  await expect(
    page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-haiku-4" })
  ).toHaveCount(0, { timeout: 2000 });
});
