/**
 * Thinking card collapsed Edit/Delete actions tests.
 *
 * Covers regression F-2 from the 2026-08-24 review: collapsed-by-default "Thoughts"
 * card has hover-reveal Edit/Delete buttons whose click handlers stopPropagation so
 * clicking them does NOT expand the card. BUT both the ConfirmDialog and EditForm
 * body were gated on `open` (useState(false)), causing:
 * - Delete on collapsed card: nothing appears (dialog gated on open=false); pending
 *   confirmDelete state then AMBUSHES the operator later when expanding (unprompted
 *   destructive dialog appears on expansion).
 * - Edit on collapsed card: nothing appears; expanding later shows an edit textarea
 *   instead of the reasoning.
 *
 * Expected post-fix behavior:
 * - Delete on collapsed card: ConfirmDialog appears immediately (card stays collapsed,
 *   dialog is a fixed overlay).
 * - Edit on collapsed card: card auto-expands AND shows the EditForm.
 * - Collapsing the card while editing drops the edit form (no stale form on re-expand).
 * - Delete and Edit still work correctly from an already-expanded card (control case).
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

async function setupWithThinkingMessage(
  page: import("@playwright/test").Page,
  messageID: string,
  thinkingText: string
) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "tca-sess", Title: "Thinking Card Session" })],
  });
  await expect(page.getByText("Thinking Card Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Thinking Card Session").first().click();
  // useWS.ts's messages_list handler drops the payload unless its SessionID
  // matches the active session (guards against a stale in-flight
  // load_messages response for a session the user has since switched away
  // from — see the "New envelope" comment in useWS.ts). makeMessage()
  // defaults SessionID to "sess-1", which never matches "tca-sess", so every
  // call here must pin it explicitly or the update is silently dropped.
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      {
        ...makeMessage({
          ID: messageID,
          Role: "assistant",
          Parts: [
            { type: "thinking", Thinking: thinkingText },
            { type: "text", Text: "Answer." },
          ],
        }),
        SessionID: "tca-sess",
      },
    ],
  });
}

test("delete click on a collapsed card shows the confirm dialog immediately", async ({ page }) => {
  await setupWithThinkingMessage(page, "tca-1", "First reasoning content");

  // Card visible and collapsed by default
  await expect(page.getByTestId("thinking-card")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("thinking-content")).not.toBeVisible();

  // Click Delete on collapsed card
  await page.getByTestId("thinking-card").hover();
  await page.getByTestId("thinking-card").getByTitle("Delete thinking").click();

  // Dialog should appear WITHOUT expanding the card
  await expect(page.getByRole("heading", { name: "Delete thinking" })).toBeVisible({ timeout: 2000 });

  // Card should STILL be collapsed while dialog is up
  await expect(page.getByTestId("thinking-content")).not.toBeVisible();
});

test("confirming delete from a collapsed card sends delete_message_part without expanding", async ({ page }) => {
  await setupWithThinkingMessage(page, "tca-2", "Second reasoning content");

  // Card visible and collapsed by default
  await expect(page.getByTestId("thinking-card")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("thinking-content")).not.toBeVisible();

  // Click Delete on collapsed card
  await page.getByTestId("thinking-card").hover();
  await page.getByTestId("thinking-card").getByTitle("Delete thinking").click();

  // Dialog should appear
  await expect(page.getByRole("heading", { name: "Delete thinking" })).toBeVisible({ timeout: 2000 });

  // Confirm deletion
  await page.locator(".modal-panel").getByRole("button", { name: "Delete" }).click();

  // Verify delete_message_part command sent
  const cmd = await waitForWSSend(page, "delete_message_part");
  expect(cmd.payload).toEqual({ messageID: "tca-2", partIndex: 0 });
});

test("cancelled delete on a collapsed card leaves no ambush on later expand", async ({ page }) => {
  await setupWithThinkingMessage(page, "tca-3", "Third reasoning content");

  // Card visible and collapsed by default
  await expect(page.getByTestId("thinking-card")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("thinking-content")).not.toBeVisible();

  // Click Delete on collapsed card
  await page.getByTestId("thinking-card").hover();
  await page.getByTestId("thinking-card").getByTitle("Delete thinking").click();

  // Dialog should appear
  await expect(page.getByRole("heading", { name: "Delete thinking" })).toBeVisible({ timeout: 2000 });

  // Cancel deletion
  await page.locator(".modal-panel").getByRole("button", { name: "Cancel" }).click();
  await expect(page.getByRole("heading", { name: "Delete thinking" })).not.toBeVisible();

  // Expand the card
  await page.getByTestId("thinking-toggle").click();

  // Should show reasoning content, NOT the dialog
  await expect(page.getByTestId("thinking-content")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("thinking-content")).toContainText("Third reasoning content");
  await expect(page.getByRole("heading", { name: "Delete thinking" })).not.toBeVisible();
});

test("edit click on a collapsed card auto-expands and shows the edit form", async ({ page }) => {
  await setupWithThinkingMessage(page, "tca-4", "Fourth reasoning content");

  // Card visible and collapsed by default
  await expect(page.getByTestId("thinking-card")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("thinking-content")).not.toBeVisible();

  // Click Edit on collapsed card
  await page.getByTestId("thinking-card").hover();
  await page.getByTestId("thinking-card").getByTitle("Edit thinking").click();

  // Card should auto-expand
  await expect(page.getByTestId("thinking-toggle")).toHaveAttribute("aria-expanded", "true");

  // Edit textarea should be visible with the thinking text
  const textarea = page.getByTestId("thinking-card").locator("textarea");
  await expect(textarea).toBeVisible({ timeout: 2000 });
  await expect(textarea).toHaveValue("Fourth reasoning content");

  // Edit and save
  await textarea.fill("Edited reasoning");
  await page.locator("button").filter({ hasText: /^Save$/ }).click();

  // Verify update_message_thinking command sent
  const cmd = await waitForWSSend(page, "update_message_thinking");
  expect(cmd.payload).toEqual({ messageID: "tca-4", thinking: "Edited reasoning" });
});

test("control: delete and edit still work from an already-expanded card", async ({ page }) => {
  await setupWithThinkingMessage(page, "tca-5", "Fifth reasoning content");

  // Expand the card FIRST
  await page.getByTestId("thinking-toggle").click();
  await expect(page.getByTestId("thinking-content")).toBeVisible({ timeout: 2000 });

  // Delete should work from expanded card
  await page.getByTestId("thinking-card").hover();
  await page.getByTestId("thinking-card").getByTitle("Delete thinking").click();
  await expect(page.getByRole("heading", { name: "Delete thinking" })).toBeVisible({ timeout: 2000 });

  // Cancel deletion
  await page.locator(".modal-panel").getByRole("button", { name: "Cancel" }).click();
  await expect(page.getByRole("heading", { name: "Delete thinking" })).not.toBeVisible();

  // Edit should work from expanded card
  await page.getByTestId("thinking-card").hover();
  await page.getByTestId("thinking-card").getByTitle("Edit thinking").click();
  await expect(page.getByTestId("thinking-card").locator("textarea")).toBeVisible({ timeout: 2000 });
});

test("collapsing the card while editing drops the edit form instead of stashing it", async ({ page }) => {
  await setupWithThinkingMessage(page, "tca-6", "Sixth reasoning content");

  // Card visible and collapsed by default
  await expect(page.getByTestId("thinking-card")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("thinking-content")).not.toBeVisible();

  // Click Edit on collapsed card (should auto-expand)
  await page.getByTestId("thinking-card").hover();
  await page.getByTestId("thinking-card").getByTitle("Edit thinking").click();

  // Textarea should be visible (auto-expanded)
  const textarea = page.getByTestId("thinking-card").locator("textarea");
  await expect(textarea).toBeVisible({ timeout: 2000 });

  // Collapse while editing
  await page.getByTestId("thinking-toggle").click();

  // Textarea should no longer be visible
  await expect(textarea).not.toBeVisible({ timeout: 2000 });

  // Re-expand
  await page.getByTestId("thinking-toggle").click();

  // Should show the reasoning content, NOT a stale edit form
  await expect(page.getByTestId("thinking-content")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("thinking-content")).toContainText("Sixth reasoning content");
  await expect(page.getByTestId("thinking-card").locator("textarea")).not.toBeVisible();
});
