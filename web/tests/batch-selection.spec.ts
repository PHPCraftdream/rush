/**
 * Batch message selection tests.
 *
 * Covers:
 *  - Checkbox appears on message hover
 *  - Selecting a message shows batch toolbar with count
 *  - Selecting multiple messages updates count
 *  - "Delete selected" triggers confirm dialog with count
 *  - Confirming sends delete_messages WS command with all IDs
 *  - "Cancel" in toolbar clears selection
 *  - Selection clears when switching sessions
 *  - Toggling checkbox deselects message
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

async function setupWithMessages(
  page: import("@playwright/test").Page,
  sessionID: string,
  messages: ReturnType<typeof makeMessage>[]
) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: sessionID, Title: "Batch Session" }),
      makeSession({ ID: "batch-other", Title: "Other Session" }),
    ],
  });
  await expect(page.getByText("Batch Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Batch Session").first().click();
  // useWS.ts's messages_list handler drops the payload unless each
  // message's SessionID matches the active session. twoMessages below uses
  // makeMessage()'s "sess-1" default, which never matches the per-test
  // sessionID ("batch-1", "batch-2", ...), so it must be pinned here.
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: messages.map((m) => ({ ...m, SessionID: sessionID })),
  });
}

const twoMessages = [
  makeMessage({ ID: "b-m1", Role: "user", Parts: [{ type: "text", Text: "First batch msg" }] }),
  // task #595: the assistant fixture needs a terminal (non-Partial) Finish
  // part. Message.tsx's selection checkbox is now gated by the same
  // streamGuardOK flag as Edit/Delete (a still-streaming assistant message
  // can never be selected, so it can never reach the bulk delete_messages
  // path) — without a Finish part here, this fixture would read as
  // still-streaming and the checkbox would never reveal on hover, which is
  // not what these batch-selection-mechanics tests are about. See
  // message-delete.spec.ts's dedicated streaming-gate tests for coverage of
  // the still-streaming case itself.
  makeMessage({
    ID: "b-m2",
    Role: "assistant",
    Parts: [
      { type: "text", Text: "Second batch msg" },
      { type: "finish", Reason: "end_turn", Message: "", Details: "" },
    ],
  }),
];

// ── Checkbox visibility ─────────────────────────────────────────────────

test("checkbox appears on message hover", async ({ page }) => {
  await setupWithMessages(page, "batch-1", twoMessages);
  await expect(page.getByText("First batch msg")).toBeVisible({ timeout: 2000 });

  const msgRow = page.getByText("First batch msg").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  // Message.tsx has no <input type="checkbox"> — selection is a custom
  // div.msg-checkbox-wrap / div.msg-checkbox pair, always mounted (to
  // reserve layout space) and toggled via opacity-0/opacity-100, not
  // display/visibility. Playwright's toBeVisible() doesn't see opacity, so
  // assert the actual reveal state via the opacity-100 class instead.
  const checkboxWrap = msgRow.locator(".msg-checkbox-wrap");
  await expect(checkboxWrap).toHaveClass(/opacity-0/);
  await msgRow.hover();
  await expect(checkboxWrap).toHaveClass(/opacity-100/);
});

// ── Selection toolbar ───────────────────────────────────────────────────

test("selecting a message shows batch toolbar with count", async ({ page }) => {
  await setupWithMessages(page, "batch-2", twoMessages);

  const msgRow = page.getByText("First batch msg").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.locator(".msg-checkbox-wrap").click();

  await expect(page.getByText("1 selected")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Delete selected")).toBeVisible();
});

test("selecting multiple messages updates count", async ({ page }) => {
  await setupWithMessages(page, "batch-3", twoMessages);

  // Select first message
  const row1 = page.getByText("First batch msg").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row1.hover();
  await row1.locator(".msg-checkbox-wrap").click();
  await expect(page.getByText("1 selected")).toBeVisible({ timeout: 2000 });

  // Select second message
  const row2 = page.getByText("Second batch msg").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row2.hover();
  await row2.locator(".msg-checkbox-wrap").click();
  await expect(page.getByText("2 selected")).toBeVisible({ timeout: 2000 });
});

// ── Batch delete ────────────────────────────────────────────────────────

test("Delete selected triggers confirm dialog with count", async ({ page }) => {
  await setupWithMessages(page, "batch-4", twoMessages);

  const row1 = page.getByText("First batch msg").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row1.hover();
  await row1.locator(".msg-checkbox-wrap").click();

  const row2 = page.getByText("Second batch msg").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row2.hover();
  await row2.locator(".msg-checkbox-wrap").click();

  await page.getByText("Delete selected").click();
  await expect(page.getByText("Delete 2 selected messages?")).toBeVisible({ timeout: 2000 });
});

test("confirming batch delete sends delete_messages with all IDs", async ({ page }) => {
  await setupWithMessages(page, "batch-5", twoMessages);

  const row1 = page.getByText("First batch msg").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row1.hover();
  await row1.locator(".msg-checkbox-wrap").click();

  const row2 = page.getByText("Second batch msg").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row2.hover();
  await row2.locator(".msg-checkbox-wrap").click();

  await page.getByText("Delete selected").click();
  await page.getByRole("button", { name: "Delete", exact: true }).click();

  const cmd = await waitForWSSend(page, "delete_messages");
  const ids = (cmd.payload as { messageIDs: string[] }).messageIDs;
  expect(ids).toContain("b-m1");
  expect(ids).toContain("b-m2");
});

// ── Cancel selection ────────────────────────────────────────────────────

test("Cancel in toolbar clears selection and hides toolbar", async ({ page }) => {
  await setupWithMessages(page, "batch-6", twoMessages);

  const row1 = page.getByText("First batch msg").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row1.hover();
  await row1.locator(".msg-checkbox-wrap").click();
  await expect(page.getByText("1 selected")).toBeVisible({ timeout: 2000 });

  // Click Cancel in the batch toolbar (not the confirm dialog)
  await page.locator("button").filter({ hasText: /^Cancel$/ }).last().click();
  await expect(page.getByText("1 selected")).not.toBeVisible({ timeout: 2000 });
});

// ── Deselect ─────────────────────────────────────────────────────────────

test("toggling checkbox deselects message", async ({ page }) => {
  await setupWithMessages(page, "batch-7", twoMessages);

  const row1 = page.getByText("First batch msg").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row1.hover();
  await row1.locator(".msg-checkbox-wrap").click();
  await expect(page.getByText("1 selected")).toBeVisible({ timeout: 2000 });

  // Click checkbox again to deselect
  await row1.locator(".msg-checkbox-wrap").click();
  await expect(page.getByText("1 selected")).not.toBeVisible({ timeout: 2000 });
});

// ── Selection clears on session switch ──────────────────────────────────

test("selection clears when switching sessions", async ({ page }) => {
  await setupWithMessages(page, "batch-8", twoMessages);

  const row1 = page.getByText("First batch msg").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row1.hover();
  await row1.locator(".msg-checkbox-wrap").click();
  await expect(page.getByText("1 selected")).toBeVisible({ timeout: 2000 });

  // Switch to other session
  await page.getByText("Other Session").click();
  await expect(page.getByText("1 selected")).not.toBeVisible({ timeout: 2000 });
});
