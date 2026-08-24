// Regression coverage for task #698 item 3 — the backdrop's onClick ignored
// the busy prop (added in task #684 for the buttons and Escape/Enter), so an
// outside click during an in-flight delete closed the dialog and tore down
// its reply listener.

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
});

test("backdrop click while a delete is in flight does not close the dialog", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "bd-busy-keep", Title: "Keep Me" }),
      makeSession({ ID: "bd-busy-die", Title: "Doomed" }),
    ],
  });
  await expect(page.getByTestId("session-title-bd-busy-die")).toBeVisible({ timeout: 3000 });

  const row = page.getByTestId("session-bd-busy-die");
  await row.hover();
  await page.getByTestId("session-delete-bd-busy-die").click();
  await page.getByText("Delete", { exact: true }).click();

  const cmd = await waitForWSSend(page, "delete_session");

  // Dialog is now busy (deleting=true) — backdrop click must be ignored.
  await page.locator(".modal-overlay").click({ position: { x: 8, y: 8 } });

  // Small timeout to catch an async close if the busy guard is missing.
  await page.waitForTimeout(300);

  // Title must still be visible — the dialog stayed mounted.
  await expect(page.getByText("Delete session")).toBeVisible();

  // Deliver an error reply; the still-mounted dialog shows it inline.
  await sendMockWSMessage(page, {
    type: "error",
    id: cmd.id,
    error: "database is locked",
  });

  // Inline error proves the reply listener survived the backdrop click.
  await expect(page.getByTestId("confirm-dialog-error")).toHaveText("database is locked", { timeout: 2000 });
});

test("CONTROL: backdrop click closes the dialog when idle", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "bd-ctrl-die", Title: "Control Delete" }),
    ],
  });
  await expect(page.getByTestId("session-title-bd-ctrl-die")).toBeVisible({ timeout: 3000 });

  const row = page.getByTestId("session-bd-ctrl-die");
  await row.hover();
  await page.getByTestId("session-delete-bd-ctrl-die").click();

  // Dialog is open but idle (Delete not clicked yet) — backdrop closes it.
  await page.locator(".modal-overlay").click({ position: { x: 8, y: 8 } });

  // Title must be hidden — the dialog closed normally.
  await expect(page.getByText("Delete session")).toBeHidden();
});
