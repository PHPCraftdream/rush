import { test, expect, Page } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeConfig, makeSession } from "./helpers/fixtures";

// Regression coverage for review 2026-08-25 P1/P2 (task #725): a UI
// action that owns busy/loading state used to register an ad-hoc WS
// reply listener with no send-failure, disconnect or timeout handling —
// one WebSocket drop and the modal stayed busy forever. The shared
// wsRequest helper (web/src/ws.ts) now rejects the promise the moment
// the request is known to be doomed, and every migrated call site
// resets its busy flag, keeps the operator's draft and shows the error.
// These tests drive the two doom paths for the worst offender
// (BuiltinProviderEditor in ProvidersModal.tsx) and the system-prompt
// save: a disconnect AFTER the frame was written, and a socket that was
// already dead when the operator clicked.

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// Kill the /ws socket from the inside, exactly like a network blip: the
// mock's close() runs the real onclose path synchronously, so the client
// emits _disconnected immediately (wsRequest rejects right away — the
// 3s assertions below double as fail-fast checks: a slow-timeout
// implementation would still be busy at 3s and fail).
async function goOffline(page: Page) {
  await page.evaluate(() => {
    const mock = ((window as unknown) as Record<string, unknown>)["__mockWS"] as { close: () => void } | null;
    if (!mock) throw new Error("mock WS not created yet");
    mock.close();
  });
}

async function framesOfType(page: Page, type: string) {
  return page.evaluate((t: string) => {
    const sent = (((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>) ?? [];
    return sent.filter((m) => m.type === t);
  }, type);
}

function builtinProviderConfig() {
  return makeConfig({
    providers: {
      anthropic: { name: "Anthropic", enabled: true, models: [] },
    },
  });
}

// Opens the peak-hours-only editor for the built-in anthropic provider
// and fills a draft API key (submit always sends set_provider_peak_hours;
// a non-empty key additionally sends set_provider_key).
async function openBuiltinEditorWithDraftKey(page: Page) {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: builtinProviderConfig() });
  await page.getByTestId("header-more-button").click();
  await page.getByTestId("header-providers-button").click();
  await expect(page.getByTestId("providers-modal")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("provider-edit-anthropic").click();
  await expect(page.getByTestId("builtin-provider-editor")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("peak-hours-only-api-key").fill("sk-offline-123");
}

test("builtin provider editor: disconnect after save fails fast, un-busies and keeps the draft", async ({ page }) => {
  await openBuiltinEditorWithDraftKey(page);
  await page.getByTestId("peak-hours-only-save").click();
  await waitForWSSend(page, "set_provider_peak_hours");
  await goOffline(page);
  await expect(page.getByTestId("builtin-provider-editor")).toContainText("Connection lost", { timeout: 3000 });
  await expect(page.getByTestId("peak-hours-only-save")).toContainText("Save", { timeout: 3000 });
  await expect(page.getByTestId("peak-hours-only-save")).toBeEnabled({ timeout: 3000 });
  await expect(page.getByTestId("peak-hours-only-api-key")).toHaveValue("sk-offline-123");
});

test("builtin provider editor: save with an already-dead socket never sends and recovers immediately", async ({ page }) => {
  await openBuiltinEditorWithDraftKey(page);
  await goOffline(page);
  await page.getByTestId("peak-hours-only-save").click();
  await expect(page.getByTestId("builtin-provider-editor")).toContainText("Not connected", { timeout: 3000 });
  await expect(page.getByTestId("peak-hours-only-save")).toContainText("Save", { timeout: 3000 });
  await expect(page.getByTestId("peak-hours-only-api-key")).toHaveValue("sk-offline-123");
  expect(await framesOfType(page, "set_provider_peak_hours")).toHaveLength(0);
  expect(await framesOfType(page, "set_provider_key")).toHaveLength(0);
});

test("system prompt save during a disconnect keeps the draft and shows the error", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "req-sp", Title: "Request Session" })],
  });
  await expect(page.getByText("Request Session").first()).toBeVisible({ timeout: 5000 });
  await page.getByText("Request Session").first().click();
  await page.getByTestId("header-more-button").click();
  await page.getByTestId("header-prompt-button").click();
  const getCmd = await waitForWSSend(page, "get_system_prompt");
  await sendMockWSMessage(page, { type: "system_prompt", id: getCmd.id, payload: { sessionID: "req-sp", content: "Original" } });
  const textarea = page.locator("div.fixed.inset-0 textarea");
  await expect(textarea).toBeVisible({ timeout: 3000 });
  await textarea.fill("Edited while offline");
  await page.locator("div.fixed.inset-0 button", { hasText: "Save" }).click();
  await waitForWSSend(page, "set_system_prompt");
  await goOffline(page);
  await expect(page.locator("div.fixed.inset-0")).toContainText("Connection lost", { timeout: 3000 });
  await expect(page.locator("div.fixed.inset-0 button", { hasText: "Save" })).toBeEnabled({ timeout: 3000 });
  await expect(textarea).toHaveValue("Edited while offline");
});
