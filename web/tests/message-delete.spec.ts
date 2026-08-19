/**
 * Single message deletion tests.
 *
 * Covers:
 *  - Delete button visible on hover
 *  - Clicking delete shows confirmation dialog
 *  - Confirm dialog Cancel dismisses without deleting
 *  - Confirm dialog Escape dismisses without deleting
 *  - Confirm dialog Delete sends delete_message WS command
 *  - message_deleted event removes message from UI
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
  messages: ReturnType<typeof makeMessage>[]
) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "del-sess", Title: "Delete Session" })],
  });
  await expect(page.getByText("Delete Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Delete Session").first().click();
  // useWS.ts's messages_list handler drops the payload unless each
  // message's SessionID matches the active session (stale in-flight
  // load_messages guard). makeMessage() defaults SessionID to "sess-1",
  // which never matches "del-sess", so it must be pinned here or every
  // assertion below times out waiting for text that was silently dropped.
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: messages.map((m) => ({ ...m, SessionID: "del-sess" })),
  });
}

// ── Delete button ────────────────────────────────────────────────────────

test("delete button appears on hover for user message", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "d-u1", Role: "user", Parts: [{ type: "text", Text: "Delete me user" }] }),
  ]);
  await expect(page.getByText("Delete me user")).toBeVisible({ timeout: 2000 });

  const msgRow = page.getByText("Delete me user").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await expect(msgRow.getByTitle("Delete")).toBeVisible({ timeout: 2000 });
});

// ── Confirm dialog ──────────────────────────────────────────────────────

test("clicking delete shows confirmation dialog", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "d-c1", Role: "user", Parts: [{ type: "text", Text: "Confirm delete" }] }),
  ]);

  const msgRow = page.getByText("Confirm delete").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Delete").click();

  await expect(page.getByText("Delete this message?")).toBeVisible({ timeout: 2000 });
  await expect(page.getByRole("button", { name: "Delete", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Cancel", exact: true })).toBeVisible();
});

test("clicking Cancel in confirm dialog dismisses without deleting", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "d-cc1", Role: "user", Parts: [{ type: "text", Text: "Cancel delete" }] }),
  ]);

  const msgRow = page.getByText("Cancel delete").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Delete").click();

  await expect(page.getByText("Delete this message?")).toBeVisible({ timeout: 2000 });
  await page.getByRole("button", { name: "Cancel" }).click();

  await expect(page.getByText("Delete this message?")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Cancel delete")).toBeVisible();

  const sent = await page.evaluate(() => {
    const s = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    return s.some((m) => m.type === "delete_message");
  });
  expect(sent).toBe(false);
});

test("pressing Escape in confirm dialog dismisses without deleting", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "d-ce1", Role: "user", Parts: [{ type: "text", Text: "Escape delete" }] }),
  ]);

  const msgRow = page.getByText("Escape delete").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Delete").click();

  await expect(page.getByText("Delete this message?")).toBeVisible({ timeout: 2000 });
  await page.keyboard.press("Escape");

  await expect(page.getByText("Delete this message?")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Escape delete")).toBeVisible();
});

test("clicking Delete in confirm dialog sends delete_message", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "d-del1", Role: "user", Parts: [{ type: "text", Text: "Really delete" }] }),
  ]);

  const msgRow = page.getByText("Really delete").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Delete").click();

  await expect(page.getByText("Delete this message?")).toBeVisible({ timeout: 2000 });
  await page.getByRole("button", { name: "Delete", exact: true }).click();

  const cmd = await waitForWSSend(page, "delete_message");
  expect((cmd.payload as { messageID: string }).messageID).toBe("d-del1");
});

// ── message_deleted event ───────────────────────────────────────────────

test("message_deleted event removes message from chat", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "d-ev1", Role: "user", Parts: [{ type: "text", Text: "Will be deleted" }] }),
    makeMessage({ ID: "d-ev2", Role: "assistant", Parts: [{ type: "text", Text: "Will remain" }] }),
  ]);

  await expect(page.getByText("Will be deleted")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Will remain")).toBeVisible();

  await sendMockWSMessage(page, {
    // useWS.ts's message_deleted handler also gates on SessionID matching
    // the active session (same guard shape as messages_list); an unscoped
    // payload is silently dropped rather than removing the message.
    type: "message_deleted",
    payload: { ID: "d-ev1", SessionID: "del-sess" },
  });

  await expect(page.getByText("Will be deleted")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Will remain")).toBeVisible();
});
