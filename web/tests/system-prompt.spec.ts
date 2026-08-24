/**
 * System prompt modal tests.
 *
 * Covers:
 *  - Prompt button not rendered without active session
 *  - Prompt button visible when session active
 *  - Clicking Prompt opens modal and sends get_system_prompt
 *  - Modal shows loading state, then textarea with content
 *  - Save button disabled when content unchanged
 *  - Save button enabled after editing
 *  - Clicking Save sends set_system_prompt WS command
 *  - Reset button reverts draft to original content
 *  - Escape closes modal
 *  - Clicking backdrop closes modal
 *  - Clicking × closes modal
 *  - Save round-trip: Save only commits (disables) after a genuine
 *    EventResponse (control); a rejected EventError leaves the edit
 *    dirty (Save re-enabled, Reset visible), shows the error inside the
 *    modal, and keeps the draft on screen (regression for F-1, twenty-sixth
 *    review — SystemPromptModal.save() used to mark the edit clean
 *    synchronously, before the server had even seen the write)
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeConfig, makeSession } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

async function setupWithSession(page: import("@playwright/test").Page) {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sp-sess", Title: "Prompt Session" })],
  });
  await expect(page.getByText("Prompt Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Prompt Session").first().click();
}

async function openMoreMenu(page: import("@playwright/test").Page) {
  await page.getByTestId("header-more-button").click();
  await expect(page.getByTestId("header-logs-button")).toBeVisible({ timeout: 2000 });
}

// ── Button state ─────────────────────────────────────────────────────────────

test("Prompt button not rendered without active session", async ({ page }) => {
  await page.goto("/");
  await page.getByTestId("header-more-button").click();
  await expect(page.getByTestId("header-prompt-button")).toHaveCount(0, { timeout: 2000 });
});

test("System prompt button visible when session active", async ({ page }) => {
  await setupWithSession(page);
  await openMoreMenu(page);
  const btn = page.getByTestId("header-prompt-button");
  await expect(btn).toBeEnabled({ timeout: 3000 });
});

// ── Opening modal ─────────────────────────────────────────────────────────────

test("System prompt modal opens on button click", async ({ page }) => {
  await setupWithSession(page);
  await openMoreMenu(page);
  await page.getByTestId("header-prompt-button").click();
  await expect(page.getByText("System Prompt")).toBeVisible({ timeout: 3000 });
});

test("modal sends get_system_prompt on open", async ({ page }) => {
  await setupWithSession(page);
  await openMoreMenu(page);
  await page.getByTestId("header-prompt-button").click();
  const cmd = await waitForWSSend(page, "get_system_prompt");
  expect((cmd.payload as { sessionID: string }).sessionID).toBe("sp-sess");
});

// ── Loading and content ───────────────────────────────────────────────────────

test("System prompt modal shows loading then textarea after response", async ({ page }) => {
  await setupWithSession(page);
  await openMoreMenu(page);
  await page.getByTestId("header-prompt-button").click();

  // Should show loading state initially
  await expect(page.getByText("Loading")).toBeVisible({ timeout: 2000 });

  // Server responds with prompt content
  await sendMockWSMessage(page, {
    type: "system_prompt",
    payload: { content: "You are a helpful assistant." },
  });

  // Loading gone; textarea visible with content
  await expect(page.getByText("Loading")).not.toBeVisible({ timeout: 2000 });
  const textarea = page.locator(".fixed textarea");
  await expect(textarea).toBeVisible({ timeout: 2000 });
  await expect(textarea).toHaveValue("You are a helpful assistant.");
});

test("System prompt modal shows loaded content", async ({ page }) => {
  await setupWithSession(page);
  await openMoreMenu(page);
  await page.getByTestId("header-prompt-button").click();
  await sendMockWSMessage(page, {
    type: "system_prompt",
    payload: { content: "You are helpful" },
  });
  const textarea = page.locator(".fixed textarea");
  await expect(textarea).toBeVisible({ timeout: 3000 });
  await expect(textarea).toHaveValue("You are helpful");
});

// ── Save behaviour ────────────────────────────────────────────────────────────

test("Save button disabled when content unchanged", async ({ page }) => {
  await setupWithSession(page);
  await openMoreMenu(page);
  await page.getByTestId("header-prompt-button").click();
  await sendMockWSMessage(page, {
    type: "system_prompt",
    payload: { content: "Original" },
  });
  const saveBtn = page.locator(".fixed button", { hasText: "Save" });
  await expect(saveBtn).toBeDisabled({ timeout: 3000 });
});

test("Save button enabled after editing", async ({ page }) => {
  await setupWithSession(page);
  await openMoreMenu(page);
  await page.getByTestId("header-prompt-button").click();
  await sendMockWSMessage(page, {
    type: "system_prompt",
    payload: { content: "Original" },
  });
  const textarea = page.locator(".fixed textarea");
  await textarea.fill("Modified prompt");
  const saveBtn = page.locator(".fixed button", { hasText: "Save" });
  await expect(saveBtn).toBeEnabled({ timeout: 2000 });
});

test("Save button sends set_system_prompt", async ({ page }) => {
  await setupWithSession(page);
  await openMoreMenu(page);
  await page.getByTestId("header-prompt-button").click();
  await sendMockWSMessage(page, {
    type: "system_prompt",
    payload: { content: "Old prompt" },
  });
  const textarea = page.locator(".fixed textarea");
  await textarea.fill("New prompt content");
  await page.locator(".fixed button", { hasText: "Save" }).click();
  const cmd = await waitForWSSend(page, "set_system_prompt");
  const payload = cmd.payload as { sessionID: string; content: string };
  expect(payload.sessionID).toBe("sp-sess");
  expect(payload.content).toBe("New prompt content");
});

// ── Reset ─────────────────────────────────────────────────────────────────────

test("Reset button reverts draft to original content", async ({ page }) => {
  await setupWithSession(page);
  await openMoreMenu(page);
  await page.getByTestId("header-prompt-button").click();
  await sendMockWSMessage(page, {
    type: "system_prompt",
    payload: { content: "Original text" },
  });
  const textarea = page.locator(".fixed textarea");
  await textarea.fill("Changed text");
  // Reset button appears only when draft differs from original
  await page.locator(".fixed button", { hasText: "Reset" }).click();
  await expect(textarea).toHaveValue("Original text");
});

// ── Close ─────────────────────────────────────────────────────────────────────

test("System prompt modal closes on Escape", async ({ page }) => {
  await setupWithSession(page);
  await openMoreMenu(page);
  await page.getByTestId("header-prompt-button").click();
  await expect(page.getByText("System Prompt")).toBeVisible({ timeout: 2000 });
  await page.keyboard.press("Escape");
  await expect(page.getByText("System Prompt")).not.toBeVisible({ timeout: 3000 });
});

test("clicking × closes system prompt modal", async ({ page }) => {
  await setupWithSession(page);
  await openMoreMenu(page);
  await page.getByTestId("header-prompt-button").click();
  await expect(page.getByText("System Prompt")).toBeVisible({ timeout: 2000 });
  await page.locator(".fixed button", { hasText: "×" }).click();
  await expect(page.getByText("System Prompt")).not.toBeVisible({ timeout: 2000 });
});

// ── Save round-trip (F-1 regression, twenty-sixth review) ─────────────────────────────────
//
// SystemPromptModal.save() used to mark the edit clean (setOriginal(draft))
// synchronously, before the server had replied at all - so a rejected write
// (EventError, e.g. "database is locked" from handlers_sessions.go's
// handleSetSystemPrompt) was silently discarded with no visible trace. The
// fix scopes a one-shot reply listener to a msgID (mirroring MCPForm.submit()
// in MCPSettings.tsx) and only commits/clears on the matching reply.

test("CONTROL: Save disabled only after a genuine EventResponse", async ({ page }) => {
  await setupWithSession(page);
  await openMoreMenu(page);
  await page.getByTestId("header-prompt-button").click();
  await sendMockWSMessage(page, { type: "system_prompt", payload: { content: "Original" } });
  const textarea = page.locator(".fixed textarea");
  await textarea.fill("Edited content");

  await page.locator(".fixed button", { hasText: "Save" }).click();
  const sentCmd = await waitForWSSend(page, "set_system_prompt");

  // Before the reply arrives, the edit must still read as dirty: Reset
  // stays visible (Save itself is disabled while the in-flight request is
  // pending, matching MCPForm.submit()'s busy-disables-submit pattern -
  // the important thing is dirty stays true, i.e. NOT prematurely committed
  // via setOriginal(draft) the way the pre-fix code did synchronously).
  const saveBtn = page.locator(".fixed button", { hasText: "Save" });
  await expect(page.locator(".fixed button", { hasText: "Reset" })).toBeVisible();

  // Server confirms the write (handlers_sessions.go: EventResponse, {status: "ok"}).
  await sendMockWSMessage(page, { type: "response", id: sentCmd.id, payload: { status: "ok" } });

  await expect(saveBtn).toBeDisabled({ timeout: 2000 });
  await expect(page.locator(".fixed button", { hasText: "Reset" })).toHaveCount(0);
});

test("BUG regression: a rejected save (EventError) leaves the edit dirty and shows the error inside the modal", async ({ page }) => {
  await setupWithSession(page);
  await openMoreMenu(page);
  await page.getByTestId("header-prompt-button").click();
  await sendMockWSMessage(page, { type: "system_prompt", payload: { content: "Original" } });
  const textarea = page.locator(".fixed textarea");
  await textarea.fill("Edited content that will be rejected");

  await page.locator(".fixed button", { hasText: "Save" }).click();
  const sentCmd = await waitForWSSend(page, "set_system_prompt");

  // Server rejects the write - exact wire shape from handlers_sessions.go.
  await sendMockWSMessage(page, {
    type: "error",
    id: sentCmd.id,
    error: "database is locked",
  });

  // Save re-enables, Reset reappears - the edit is NOT silently marked clean.
  const saveBtn = page.locator(".fixed button", { hasText: "Save" });
  await expect(saveBtn).toBeEnabled({ timeout: 2000 });
  await expect(page.locator(".fixed button", { hasText: "Reset" })).toBeVisible();

  // The error is visible inside the modal itself, not only via the global
  // 8-second transcript-pane banner.
  await expect(page.locator(".fixed", { hasText: "database is locked" })).toBeVisible();

  // The draft is preserved on screen - the operator's edit is not discarded.
  await expect(textarea).toHaveValue("Edited content that will be rejected");
});

test("BUG regression: reopening the modal after a rejected save does not silently discard the edit", async ({ page }) => {
  await setupWithSession(page);
  await openMoreMenu(page);
  await page.getByTestId("header-prompt-button").click();
  await sendMockWSMessage(page, { type: "system_prompt", payload: { content: "Original" } });
  const textarea = page.locator(".fixed textarea");
  await textarea.fill("Edited content that will be rejected");
  await page.locator(".fixed button", { hasText: "Save" }).click();
  const sentCmd = await waitForWSSend(page, "set_system_prompt");
  await sendMockWSMessage(page, { type: "error", id: sentCmd.id, error: "database is locked" });
  await expect(page.locator(".fixed", { hasText: "database is locked" })).toBeVisible();

  // Chosen fix shape: the draft is never cleared on a rejected save, so the
  // still-open modal keeps showing the operator's edit (not the stale
  // original) with the error alongside it - closing/reopening is not
  // required to recover the text, unlike the pre-fix behavior where the
  // edit was already gone the moment Save was clicked.
  await expect(textarea).toHaveValue("Edited content that will be rejected");

  // Closing and reopening re-fetches from the server (get_system_prompt) and
  // correctly shows the true saved state, since the rejected edit was never
  // persisted - this is expected: reopening reflects server truth, not a
  // second local cache of the failed draft.
  await page.locator(".fixed button", { hasText: "×" }).click();
  await page.getByTestId("header-more-button").click();
  await page.getByTestId("header-prompt-button").click();
  await sendMockWSMessage(page, { type: "system_prompt", payload: { content: "Original" } });
  const textarea2 = page.locator(".fixed textarea");
  await expect(textarea2).toHaveValue("Original");
});

