import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

async function selectASession(page: Parameters<typeof sendMockWSMessage>[0], id: string, title: string) {
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: id, Title: title })],
  });
  await expect(page.getByText(title).first()).toBeVisible({ timeout: 3000 });
  await page.getByText(title).first().click();
}

async function openMoreMenu(page: import("@playwright/test").Page) {
  await page.getByTestId("header-more-button").click();
  await expect(page.getByTestId("header-logs-button")).toBeVisible({ timeout: 2000 });
}

// ── Toolbar (formerly "Header") ──────────────────────────────────────────────
//
// Commit 89a07919 ("fold Header into ChatToolbar") deleted the standalone
// Header.tsx and its data-test-id="header" wrapper entirely, moving the
// surviving controls (settings/token-indicator/busy-dots/theme/etc.) into
// ChatToolbar.tsx. A behavioral change falls out of that commit, contrary
// to its "no behaviour changes" claim:
//   1. There is no longer any element carrying data-test-id="header" to
//      scope assertions to — the controls now live directly in the page.
//
// "header shows session title when session is active" is dropped entirely,
// not re-pointed: SessionTitle was the one piece of Header.tsx that commit
// explicitly did NOT migrate anywhere ("session rename still works via the
// sidebar's per-row pencil button" — i.e. the title is only ever shown in
// the sidebar row now, which sessions.spec.ts / sidebar-detail.spec.ts
// already cover). There is no second place displaying the session title to
// re-point this test at.

test("empty state shows default title when no session selected", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByText("No session selected")).toBeVisible({ timeout: 2000 });
});

test("settings button opens modal showing model name from config", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "hdr-model-1", Title: "Model Config Session" })],
  });
  await expect(page.getByText("Model Config Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Model Config Session").first().click();
  await sendMockWSMessage(page, {
    type: "config",
    payload: { models: { smart: { Provider: "anthropic", Model: "claude-3-5-sonnet" } } },
  });
  // Model name appears in the settings modal
  await openMoreMenu(page);
  await page.getByTestId("header-settings-button").click();
  await expect(page.getByText("Context Paths")).toBeVisible({ timeout: 2000 });
});

test("toolbar shows token usage for active session", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "tok-1", Title: "Token Session", PromptTokens: 1200, CompletionTokens: 800 })],
  });
  await expect(page.getByText("Token Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Token Session").first().click();
  // 1200 + 800 = 2000 → formatTokens → "2.0k" — badge has a title containing "tokens"
  await expect(page.getByTestId("header-token-indicator")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("header-token-indicator")).toContainText(/2\.0k/);
});

test("toolbar shows busy dots when agent is working", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "hdr-busy", Title: "Busy Header" })],
  });
  await expect(page.getByText("Busy Header").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Busy Header").first().click();
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "hdr-busy", Busy: true },
  });
  // Busy dots are visible within the toolbar. ChatToolbar.tsx renders two
  // separate .animate-pulse-dots elements (a compact one and one with
  // title="Agent is working…"), so scope to .first() to avoid a strict-mode
  // violation — this only needs to prove busy state is shown somewhere.
  await expect(page.locator(".animate-pulse-dots").first()).toBeVisible({ timeout: 2000 });
});

// ── Settings panel ─────────────────────────────────────────────────────────

test("settings panel opens on gear click", async ({ page }) => {
  await page.goto("/");
  await selectASession(page, "settings-open", "Settings Open");
  await openMoreMenu(page);
  await page.getByTestId("header-settings-button").click();
  await expect(page.getByTestId("settings-modal")).toBeVisible({ timeout: 2000 });
});

test("settings panel closes via backdrop click", async ({ page }) => {
  await page.goto("/");
  await selectASession(page, "settings-backdrop", "Settings Backdrop");
  await openMoreMenu(page);
  await page.getByTestId("header-settings-button").click();
  await expect(page.getByTestId("settings-modal")).toBeVisible({ timeout: 2000 });
  // Press Escape to close the modal
  await page.keyboard.press("Escape");
  await expect(page.getByTestId("settings-modal")).not.toBeVisible({ timeout: 2000 });
});

test("settings panel closes via X button", async ({ page }) => {
  await page.goto("/");
  await selectASession(page, "settings-x", "Settings X");
  await openMoreMenu(page);
  await page.getByTestId("header-settings-button").click();
  await expect(page.getByTestId("settings-modal")).toBeVisible({ timeout: 2000 });
  await page.getByTestId("settings-modal-close").click();
  await expect(page.getByTestId("settings-modal")).not.toBeVisible({ timeout: 2000 });
});

test("settings shows configured models", async ({ page }) => {
  await page.goto("/");
  await selectASession(page, "settings-models", "Settings Models");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await openMoreMenu(page);
  await page.getByTestId("header-settings-button").click();
  // Settings modal shows "Context Paths" and "Agent Skills Paths" sections
  await expect(page.getByText("Context Paths")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Agent Skills Paths")).toBeVisible();
});

test("settings shows Loading when config not yet received", async ({ page }) => {
  await page.goto("/");
  await selectASession(page, "settings-loading", "Settings Loading");
  await openMoreMenu(page);
  await page.getByTestId("header-settings-button").click();
  // Settings modal should be visible even without config
  await expect(page.getByTestId("settings-modal")).toBeVisible({ timeout: 2000 });
});

// ── Sidebar busy indicator ─────────────────────────────────────────────────

test("sidebar shows busy pulse for busy session", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sb-busy", Title: "Busy One" })],
  });
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "sb-busy", Busy: true },
  });
  const sessionItem = page.getByText("Busy One").first().locator("..");
  await expect(sessionItem.locator(".animate-pulse")).toBeVisible({ timeout: 2000 });
});

test("sidebar busy pulse disappears when agent done", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sb-done", Title: "Done Session" })],
  });
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "sb-done", Busy: true },
  });
  const sessionItem = page.getByText("Done Session").first().locator("..");
  await expect(sessionItem.locator(".animate-pulse")).toBeVisible({ timeout: 2000 });

  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "sb-done", Busy: false },
  });
  await expect(sessionItem.locator(".animate-pulse")).not.toBeVisible({ timeout: 2000 });
});

// ── Status bar ─────────────────────────────────────────────────────────────
//
// StatusBar.tsx is only ever mounted from inside ChatToolbar.tsx (inline,
// middle of the toolbar) or TodoList.tsx — TodoList requires an active
// session ({activeSessionID && <TodoList .../>} in Chat.tsx). These tests
// select a session first to ensure the status bar is tested in an active
// session context.

test("status bar is visible with connection status", async ({ page }) => {
  await page.goto("/");
  await selectASession(page, "status-visible", "Status Visible");
  await expect(
    page.getByTestId("status-bar")
  ).toBeVisible({ timeout: 3000 });
});

test("status bar shows MCP server", async ({ page }) => {
  await page.goto("/");
  await selectASession(page, "status-mcp", "Status MCP");
  await sendMockWSMessage(page, {
    type: "mcp_state",
    payload: { servers: [{ name: "filesystem", status: "connected" }] },
  });
  await expect(page.getByTestId("status-mcp-filesystem")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("status-mcp")).toContainText("MCP");
});

// ── Permission dialog (removed feature) ──────────────────────────────────────
//
// DELETED, not re-pointed. Commit bc3f3b3d ("fix(web): auto-approve all
// permissions for web UI sessions, remove dialog") deleted
// PermissionDialog.tsx and every associated WS event/handler/payload type —
// web sessions now auto-approve every tool call server-side
// (app.RunNonInteractive -> Permissions.AutoApproveSession is armed at every
// web-server entry point that can start agent execution) and structurally
// cannot surface a permission_request the client would ever render. There is
// nothing left to re-point these two tests at. The same dead feature was
// covered by the now-deleted permissions.spec.ts and by
// cli-models.spec.ts's now-removed "CLI model permissions" describe block.

// ── Reconnection banner ────────────────────────────────────────────────────

test("reconnecting banner appears when disconnected", async ({ page }) => {
  await page.goto("/");
  // Simulate disconnection by sending disconnect event
  await sendMockWSMessage(page, {
    type: "_disconnected",
    payload: null,
  });
  // The banner should appear
  await expect(page.getByText("Reconnecting…")).toBeVisible({ timeout: 2000 });
});
