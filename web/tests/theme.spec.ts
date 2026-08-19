import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeConfig, makeSession } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// The theme toggle used to live in the standalone Header.tsx, which rendered
// unconditionally. Commit 89a07919 ("fold Header into ChatToolbar") moved it
// into ChatToolbar.tsx, which has `if (!activeSessionID) return null;` — so,
// contrary to that commit's "no behaviour changes" claim, the toggle (and
// every other control that moved with it) is now unreachable until a session
// is selected. That gating is a real product behavior, not a test artifact,
// so these tests select a session first rather than asserting the toggle is
// reachable with none selected — see docs/checkpoints or task #567 report for
// the regression writeup; re-pointing here does not paper over it.

async function selectSession(page: Parameters<typeof sendMockWSMessage>[0], title: string) {
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "theme-sess", Title: title })],
  });
  await expect(page.getByText(title).first()).toBeVisible({ timeout: 3000 });
  await page.getByText(title).first().click();
}

// ── Theme toggle visibility ──────────────────────────────────────────────────

test("Theme toggle button visible in toolbar", async ({ page }) => {
  await page.goto("/");
  await selectSession(page, "Theme Session 1");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ theme: "light" }),
  });
  await expect(
    page.getByTitle("Switch to dark theme")
  ).toBeVisible({ timeout: 3000 });
});

// ── Light → dark ─────────────────────────────────────────────────────────────

test("Clicking theme toggle sends set_theme command", async ({ page }) => {
  await page.goto("/");
  await selectSession(page, "Theme Session 2");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ theme: "light" }),
  });
  await expect(page.getByTitle("Switch to dark theme")).toBeVisible({
    timeout: 3000,
  });
  await page.getByTitle("Switch to dark theme").click();
  const msg = await waitForWSSend(page, "set_theme");
  const payload = msg.payload as Record<string, unknown>;
  expect(payload.theme).toBe("dark");
});

// ── Dark theme config ────────────────────────────────────────────────────────

test("Dark theme config shows light toggle", async ({ page }) => {
  await page.goto("/");
  await selectSession(page, "Theme Session 3");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ theme: "dark" }),
  });
  await expect(
    page.getByTitle("Switch to light theme")
  ).toBeVisible({ timeout: 3000 });
});

test("Clicking theme toggle in dark mode sends light theme", async ({
  page,
}) => {
  await page.goto("/");
  await selectSession(page, "Theme Session 4");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ theme: "dark" }),
  });
  await expect(page.getByTitle("Switch to light theme")).toBeVisible({
    timeout: 3000,
  });
  await page.getByTitle("Switch to light theme").click();
  const msg = await waitForWSSend(page, "set_theme");
  const payload = msg.payload as Record<string, unknown>;
  expect(payload.theme).toBe("light");
});

// ── Dark class on document ───────────────────────────────────────────────────

test("Theme applies dark class to document", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({ theme: "dark" }),
  });
  // Wait for react to apply the class
  await page.waitForFunction(
    () => document.documentElement.classList.contains("dark"),
    { timeout: 5000 }
  );
  const hasDark = await page.evaluate(() =>
    document.documentElement.classList.contains("dark")
  );
  expect(hasDark).toBe(true);
});
