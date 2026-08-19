/**
 * Detailed status bar tests.
 *
 * Covers:
 *  - Connection status dot color (green/red)
 *  - MCP server statuses and dot colors
 *  - MCP not shown when no servers
 *
 * LSP coverage was deleted entirely — see the note above the (former) LSP
 * section below for why.
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// StatusBar.tsx (data-test-id="status-bar", holding status-connection and
// status-mcp-*) is only ever mounted from inside ChatToolbar.tsx (inline,
// in the `foreignOwned` early-return branch only) or TodoList.tsx — and
// TodoList itself is only rendered when `{activeSessionID && <TodoList
// .../>}` in Chat.tsx. There is no standalone always-mounted status footer,
// so every test here must select a session first (mirrors the identical
// fix already applied in ui.spec.ts's "Status bar" section).
async function selectASession(page: import("@playwright/test").Page, id: string, title: string) {
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: id, Title: title })],
  });
  await expect(page.getByText(title).first()).toBeVisible({ timeout: 3000 });
  await page.getByText(title).first().click();
}

// ── Connection status ──────────────────────────────────────────────────

test("shows Connected with green dot after WS connects", async ({ page }) => {
  await page.goto("/");
  await selectASession(page, "status-conn", "Status Conn Session");
  await expect(page.getByTestId("status-connection")).toContainText("Connected", { timeout: 3000 });
  const dot = page.getByTestId("status-connection").locator("span.rounded-full");
  await expect(dot).toHaveClass(/bg-green/);
});

// ── LSP states ─────────────────────────────────────────────────────────
//
// Deleted (8 tests): "multiple LSP servers displayed", "LSP state update
// replaces existing entry", "LSP running/ready state shows green dot",
// "LSP starting state shows yellow dot", "LSP error state shows red dot",
// "LSP section not shown when no servers", plus the two MCP-vs-LSP overlap
// tests that referenced lsp_state. There is zero "lsp"/"LSP" anywhere in
// src/ (confirmed by grep across the whole tree) — internal/lsp was
// removed from the Go backend and StatusBar.tsx was rewritten to only
// render connection + MCP. This matches CLAUDE.md's "What This Fork
// REMOVED" table: LSP integration is fully gone, not just hidden.

// ── MCP states ─────────────────────────────────────────────────────────

test("MCP connected server shows green dot", async ({ page }) => {
  await page.goto("/");
  await selectASession(page, "status-mcp-1", "Status MCP1 Session");
  await sendMockWSMessage(page, {
    type: "mcp_state",
    payload: { servers: [{ name: "filesystem", status: "connected" }] },
  });

  const mcpItem = page.getByTestId("status-mcp-filesystem");
  await expect(mcpItem).toBeVisible({ timeout: 2000 });
  await expect(mcpItem.locator("span.rounded-full")).toHaveClass(/bg-green/);
});

test("MCP connecting server shows yellow dot", async ({ page }) => {
  await page.goto("/");
  await selectASession(page, "status-mcp-2", "Status MCP2 Session");
  await sendMockWSMessage(page, {
    type: "mcp_state",
    payload: { servers: [{ name: "db-server", status: "connecting" }] },
  });

  const mcpItem = page.getByTestId("status-mcp-db-server");
  await expect(mcpItem).toBeVisible({ timeout: 2000 });
  await expect(mcpItem.locator("span.rounded-full")).toHaveClass(/bg-yellow/);
});

test("MCP error server shows red dot", async ({ page }) => {
  await page.goto("/");
  await selectASession(page, "status-mcp-3", "Status MCP3 Session");
  await sendMockWSMessage(page, {
    type: "mcp_state",
    payload: { servers: [{ name: "broken", status: "error" }] },
  });

  const mcpItem = page.getByTestId("status-mcp-broken");
  await expect(mcpItem).toBeVisible({ timeout: 2000 });
  await expect(mcpItem.locator("span.rounded-full")).toHaveClass(/bg-red/);
});

test("MCP section not shown when no servers", async ({ page }) => {
  await page.goto("/");
  await selectASession(page, "status-mcp-4", "Status MCP4 Session");
  await expect(page.getByTestId("status-connection")).toContainText("Connected", { timeout: 3000 });
  await expect(page.getByTestId("status-mcp")).not.toBeVisible({ timeout: 1000 });
});

test("MCP section not shown when servers array is empty", async ({ page }) => {
  await page.goto("/");
  await selectASession(page, "status-mcp-5", "Status MCP5 Session");
  await sendMockWSMessage(page, {
    type: "mcp_state",
    payload: { servers: [] },
  });
  await expect(page.getByTestId("status-mcp")).not.toBeVisible({ timeout: 1000 });
});

test("multiple MCP servers displayed", async ({ page }) => {
  await page.goto("/");
  await selectASession(page, "status-mcp-6", "Status MCP6 Session");
  await sendMockWSMessage(page, {
    type: "mcp_state",
    payload: {
      servers: [
        { name: "filesystem", status: "connected" },
        { name: "database", status: "connecting" },
      ],
    },
  });

  await expect(page.getByTestId("status-mcp-filesystem")).toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("status-mcp-database")).toBeVisible();
});
