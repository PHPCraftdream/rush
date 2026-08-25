/**
 * Checkbox right-click context menu tests.
 *
 * Covers:
 *  - Right-click on the selection checkbox opens a menu with two options
 *  - "+ Select all above" selects the clicked message and everything before it
 *  - "+ Select all below" selects the clicked message and everything after it
 *  - Selection is additive, not a replace (existing selection survives)
 *  - Menu closes on outside click / Escape without changing selection
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

const threeMessages = [
  makeMessage({ ID: "cm-1", Role: "user", Parts: [{ type: "text", Text: "First message" }] }),
  makeMessage({
    ID: "cm-2",
    Role: "assistant",
    Parts: [
      { type: "text", Text: "Second message" },
      { type: "finish", Reason: "end_turn", Message: "", Details: "" },
    ],
  }),
  makeMessage({ ID: "cm-3", Role: "user", Parts: [{ type: "text", Text: "Third message" }] }),
];

async function setupWithMessages(page: import("@playwright/test").Page) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "ctxmenu-sess", Title: "CtxMenu Session" })],
  });
  await expect(page.getByText("CtxMenu Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("CtxMenu Session").first().click();
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: threeMessages.map((m) => ({ ...m, SessionID: "ctxmenu-sess" })),
  });
  await expect(page.getByText("Second message")).toBeVisible({ timeout: 2000 });
}

test("right-click on checkbox opens a menu with both select options", async ({ page }) => {
  await setupWithMessages(page);

  const row = page.getByText("Second message").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row.hover();
  await row.locator(".msg-checkbox-wrap").click({ button: "right" });

  await expect(page.getByText("+ Select all above")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("+ Select all below")).toBeVisible();
});

test("Select all above selects the clicked message and everything before it", async ({ page }) => {
  await setupWithMessages(page);

  const row = page.getByText("Second message").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row.hover();
  await row.locator(".msg-checkbox-wrap").click({ button: "right" });
  await page.getByText("+ Select all above").click();

  await expect(page.getByText("2 selected")).toBeVisible({ timeout: 2000 });
});

test("Select all below selects the clicked message and everything after it", async ({ page }) => {
  await setupWithMessages(page);

  const row = page.getByText("Second message").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row.hover();
  await row.locator(".msg-checkbox-wrap").click({ button: "right" });
  await page.getByText("+ Select all below").click();

  await expect(page.getByText("2 selected")).toBeVisible({ timeout: 2000 });
});

test("selection is additive: a pre-existing selection survives Select all below", async ({ page }) => {
  await setupWithMessages(page);

  const firstRow = page.getByText("First message").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await firstRow.hover();
  await firstRow.locator(".msg-checkbox-wrap").click();
  await expect(page.getByText("1 selected")).toBeVisible({ timeout: 2000 });

  const secondRow = page.getByText("Second message").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await secondRow.hover();
  await secondRow.locator(".msg-checkbox-wrap").click({ button: "right" });
  await page.getByText("+ Select all below").click();

  // cm-1 (already selected) + cm-2, cm-3 (newly added) = 3, not 2 -- proves
  // the pre-existing selection wasn't replaced.
  await expect(page.getByText("3 selected")).toBeVisible({ timeout: 2000 });
});

test("clicking outside the menu closes it without changing selection", async ({ page }) => {
  await setupWithMessages(page);

  const row = page.getByText("Second message").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row.hover();
  await row.locator(".msg-checkbox-wrap").click({ button: "right" });
  await expect(page.getByText("+ Select all above")).toBeVisible({ timeout: 2000 });

  await page.mouse.click(10, 10);

  await expect(page.getByText("+ Select all above")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("selected")).not.toBeVisible();
});

test("Escape closes the menu without changing selection", async ({ page }) => {
  await setupWithMessages(page);

  const row = page.getByText("Second message").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row.hover();
  await row.locator(".msg-checkbox-wrap").click({ button: "right" });
  await expect(page.getByText("+ Select all above")).toBeVisible({ timeout: 2000 });

  await page.keyboard.press("Escape");

  await expect(page.getByText("+ Select all above")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("selected")).not.toBeVisible();
});
