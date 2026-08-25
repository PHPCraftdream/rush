/**
 * Message retry tests.
 *
 * "Retry" re-sends a user message: the server (handleRerunMessage) cancels
 * any in-flight turn, deletes the target message and everything after it,
 * then re-runs the agent with the same prompt. The client sends
 * rerun_message with just the target messageID -- the server does the
 * cancel/delete/resend atomically.
 *
 * Covers:
 *  - Retry button visible on hover for EVERY user message, not just the last
 *  - Retrying the last user message (nothing below) sends rerun_message
 *    immediately, no confirmation
 *  - Retrying an earlier user message (something below) shows a confirm
 *    dialog warning that messages below will be deleted
 *  - Confirm dialog Cancel dismisses without sending rerun_message
 *  - Confirm dialog Escape dismisses without sending rerun_message
 *  - Confirm dialog Retry sends rerun_message with the right messageID
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
    payload: [makeSession({ ID: "retry-sess", Title: "Retry Session" })],
  });
  await expect(page.getByText("Retry Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Retry Session").first().click();
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: messages.map((m) => ({ ...m, SessionID: "retry-sess" })),
  });
}

// ── Retry button visibility ─────────────────────────────────────────────

test("retry button appears on hover for a user message that is NOT the last one", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "r-u1", Role: "user", Parts: [{ type: "text", Text: "First prompt" }] }),
    makeMessage({
      ID: "r-a1",
      Role: "assistant",
      Parts: [
        { type: "text", Text: "First reply" },
        { type: "finish", Reason: "end_turn", Message: "", Details: "" },
      ],
    }),
    makeMessage({ ID: "r-u2", Role: "user", Parts: [{ type: "text", Text: "Second prompt" }] }),
  ]);
  await expect(page.getByText("First prompt")).toBeVisible({ timeout: 2000 });

  const msgRow = page.getByText("First prompt").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await expect(msgRow.getByTitle("Retry")).toBeVisible({ timeout: 2000 });
});

test("retry button appears on hover for the last user message too", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "r-only", Role: "user", Parts: [{ type: "text", Text: "Only prompt" }] }),
  ]);
  await expect(page.getByText("Only prompt")).toBeVisible({ timeout: 2000 });

  const msgRow = page.getByText("Only prompt").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await expect(msgRow.getByTitle("Retry")).toBeVisible({ timeout: 2000 });
});

// ── Last user message: no confirmation needed (nothing below to lose) ───

test("retrying the last user message sends rerun_message immediately, no confirm dialog", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "r-last1", Role: "user", Parts: [{ type: "text", Text: "Last prompt" }] }),
  ]);
  await expect(page.getByText("Last prompt")).toBeVisible({ timeout: 2000 });

  const msgRow = page.getByText("Last prompt").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Retry").click();

  const cmd = await waitForWSSend(page, "rerun_message");
  expect((cmd.payload as { messageID: string }).messageID).toBe("r-last1");
  await expect(page.getByText("Retry message")).not.toBeVisible({ timeout: 500 });
});

// ── Earlier user message: confirmation required (deletes everything below) ─

test("retrying an earlier user message shows a confirm dialog warning about deleted messages", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "r-early1", Role: "user", Parts: [{ type: "text", Text: "Early prompt" }] }),
    makeMessage({
      ID: "r-early-reply",
      Role: "assistant",
      Parts: [
        { type: "text", Text: "Early reply" },
        { type: "finish", Reason: "end_turn", Message: "", Details: "" },
      ],
    }),
    makeMessage({ ID: "r-early2", Role: "user", Parts: [{ type: "text", Text: "Later prompt" }] }),
  ]);

  const msgRow = page.getByText("Early prompt").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Retry").click();

  await expect(page.getByText("Retry message")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText(/deleted, then resent/)).toBeVisible();
  await expect(page.getByRole("button", { name: "Retry", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Cancel", exact: true })).toBeVisible();

  // Nothing sent yet -- confirmation is pending.
  const sentBeforeConfirm = await page.evaluate(() => {
    const s = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    return s.some((m) => m.type === "rerun_message");
  });
  expect(sentBeforeConfirm).toBe(false);
});

test("clicking Cancel in retry confirm dialog dismisses without sending rerun_message", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "r-cancel1", Role: "user", Parts: [{ type: "text", Text: "Cancel-retry prompt" }] }),
    makeMessage({
      ID: "r-cancel-reply",
      Role: "assistant",
      Parts: [
        { type: "text", Text: "Reply that would be lost" },
        { type: "finish", Reason: "end_turn", Message: "", Details: "" },
      ],
    }),
  ]);

  const msgRow = page.getByText("Cancel-retry prompt").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Retry").click();

  await expect(page.getByText("Retry message")).toBeVisible({ timeout: 2000 });
  await page.getByRole("button", { name: "Cancel" }).click();

  await expect(page.getByText("Retry message")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Reply that would be lost")).toBeVisible();

  const sent = await page.evaluate(() => {
    const s = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    return s.some((m) => m.type === "rerun_message");
  });
  expect(sent).toBe(false);
});

test("pressing Escape in retry confirm dialog dismisses without sending rerun_message", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "r-esc1", Role: "user", Parts: [{ type: "text", Text: "Escape-retry prompt" }] }),
    makeMessage({
      ID: "r-esc-reply",
      Role: "assistant",
      Parts: [
        { type: "text", Text: "Reply still here" },
        { type: "finish", Reason: "end_turn", Message: "", Details: "" },
      ],
    }),
  ]);

  const msgRow = page.getByText("Escape-retry prompt").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Retry").click();

  await expect(page.getByText("Retry message")).toBeVisible({ timeout: 2000 });
  await page.keyboard.press("Escape");

  await expect(page.getByText("Retry message")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Reply still here")).toBeVisible();
});

test("confirming retry sends rerun_message with the target messageID", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "r-confirm1", Role: "user", Parts: [{ type: "text", Text: "Confirm-retry prompt" }] }),
    makeMessage({
      ID: "r-confirm-reply",
      Role: "assistant",
      Parts: [
        { type: "text", Text: "Reply to be deleted" },
        { type: "finish", Reason: "end_turn", Message: "", Details: "" },
      ],
    }),
    makeMessage({ ID: "r-confirm2", Role: "user", Parts: [{ type: "text", Text: "Second prompt to be deleted" }] }),
  ]);

  const msgRow = page.getByText("Confirm-retry prompt").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Retry").click();

  await expect(page.getByText("Retry message")).toBeVisible({ timeout: 2000 });
  await page.getByRole("button", { name: "Retry", exact: true }).click();

  const cmd = await waitForWSSend(page, "rerun_message");
  expect((cmd.payload as { messageID: string }).messageID).toBe("r-confirm1");
});
