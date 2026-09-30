/**
 * Live-work tabbed panel (task #1059): Tasks / Commands / Agents.
 *
 * The server-side emitter that populates real commands/agents lands in
 * #1058 -- these tests drive the client entirely off synthetic
 * `session_live_work` WS events via the mock-WS fixtures, exercising the
 * tab machinery, the jump-to-tool-call anchor/expand signal, and the
 * activation request independently of that later server work.
 *
 * Verifies:
 *  1. Tasks is always visible; Commands/Agents are hidden while empty.
 *  2. Tab titles carry the live item count.
 *  3. A tab appears and disappears as session_live_work events arrive.
 *  4. Clicking a Commands item scrolls to and expands its (multi-action,
 *     initially collapsed) tool-activity-group row.
 *  5. Clicking an Agents item expands its SubAgentBlock.
 *  6. Selecting a tab that then empties falls back to Tasks.
 *  7. get_session_live_work is sent when a session becomes active.
 *  8. An unknown-command error reply for get_session_live_work does not
 *     surface the global error banner.
 *  9. Clicking an item with no matching tool call in the loaded transcript
 *     shows a short "not found" hint instead of failing silently.
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession, makeMessage, makeConfig } from "./helpers/fixtures";

const F = { type: "finish", Reason: "end_turn", Message: "", Details: "" };

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
});

async function selectSession(page: import("@playwright/test").Page, id: string, title: string) {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [makeSession({ ID: id, Title: title })] });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText(title).first()).toBeVisible({ timeout: 3000 });
  await page.getByText(title).first().click();
}

async function sendLiveWork(
  page: import("@playwright/test").Page,
  sessionID: string,
  commands: unknown[] = [],
  agents: unknown[] = [],
) {
  await sendMockWSMessage(page, { type: "session_live_work", payload: { sessionID, commands, agents } });
}

// ── 1/2/3. Tabs visibility + counts ──────────────────────────────────────────

test("Tasks tab is always visible; Commands/Agents hidden while empty", async ({ page }) => {
  await selectSession(page, "lw-empty", "LW Empty");
  await expect(page.getByTestId("live-work-tab-tasks")).toBeVisible();
  await expect(page.getByTestId("live-work-tab-commands")).toHaveCount(0);
  await expect(page.getByTestId("live-work-tab-agents")).toHaveCount(0);
});

test("tab titles show the live item count, and a tab disappears once its list empties", async ({ page }) => {
  await selectSession(page, "lw-counts", "LW Counts");
  await sendLiveWork(
    page,
    "lw-counts",
    [
      { toolCallID: "tc-a", toolName: "bash", title: "echo a", startedAt: Date.now() - 3000 },
      { toolCallID: "tc-b", toolName: "run_command", title: "echo b", startedAt: Date.now() - 3000 },
    ],
    [{ toolCallID: "tc-c", toolName: "agent", title: "Investigate flake", startedAt: Date.now() - 3000 }],
  );

  await expect(page.getByTestId("live-work-tab-commands")).toHaveText("Commands 2");
  await expect(page.getByTestId("live-work-tab-agents")).toHaveText("Agents 1");

  // Elapsed time is rendered per item (not just the title).
  await page.getByTestId("live-work-tab-commands").click();
  await expect(page.getByTestId("live-work-row").first()).toContainText(/\d+s/);

  // A fresh empty snapshot removes the tab entirely.
  await sendLiveWork(page, "lw-counts", [], [{ toolCallID: "tc-c", toolName: "agent", title: "Investigate flake", startedAt: Date.now() }]);
  await expect(page.getByTestId("live-work-tab-commands")).toHaveCount(0);
});

// ── 4. Jump-to-tool-call: Commands (ToolActivityGroup accordion) ────────────

test("clicking a Commands item scrolls to and expands its tool call", async ({ page }) => {
  await selectSession(page, "lw-jump", "LW Jump");

  // Two bash calls in one burst (forces the accordion wrapper, not the
  // single-action inline bypass), followed by a final prose message so the
  // group is NOT the most recent render item and starts collapsed.
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessage({ ID: "u1", SessionID: "lw-jump", Role: "user", Parts: [{ type: "text", Text: "go" }] }),
      makeMessage({
        ID: "d1",
        SessionID: "lw-jump",
        Role: "assistant",
        Parts: [
          { type: "tool_call", ID: "tc-cmd-1", Name: "bash", Input: '{"command":"echo one"}', Finished: true },
          { type: "tool_call", ID: "tc-cmd-2", Name: "bash", Input: '{"command":"echo two"}', Finished: true },
          F,
        ],
      }),
      makeMessage({ ID: "td1", SessionID: "lw-jump", Role: "tool", Parts: [{ type: "tool_result", ToolCallID: "tc-cmd-1", Name: "bash", Content: "one", IsError: false }] }),
      makeMessage({ ID: "td2", SessionID: "lw-jump", Role: "tool", Parts: [{ type: "tool_result", ToolCallID: "tc-cmd-2", Name: "bash", Content: "two", IsError: false }] }),
      makeMessage({ ID: "d2", SessionID: "lw-jump", Role: "assistant", Parts: [{ type: "text", Text: "Done." }, F] }),
    ],
  });

  const groupToggle = page.getByTestId("tool-activity-toggle");
  await expect(groupToggle).toHaveAttribute("aria-expanded", "false");

  await sendLiveWork(page, "lw-jump", [{ toolCallID: "tc-cmd-2", toolName: "bash", title: "echo two", startedAt: Date.now() - 1000 }]);
  await page.getByTestId("live-work-tab-commands").click();
  await page.getByTestId("live-work-row-jump").filter({ hasText: "echo two" }).click();

  const row = page.locator('[data-tool-call-id="tc-cmd-2"]');
  await expect(row).toBeInViewport();
  await expect(row.locator('[data-test-id="action-row-toggle"]')).toHaveAttribute("aria-expanded", "true");
  await expect(groupToggle).toHaveAttribute("aria-expanded", "true");
});

// ── 5. Jump-to-tool-call: Agents (SubAgentBlock) ────────────────────────────

test("clicking an Agents item expands its SubAgentBlock", async ({ page }) => {
  await selectSession(page, "lw-agent-jump", "LW Agent Jump");

  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessage({ ID: "u1", SessionID: "lw-agent-jump", Role: "user", Parts: [{ type: "text", Text: "delegate" }] }),
      makeMessage({
        ID: "d1",
        SessionID: "lw-agent-jump",
        Role: "assistant",
        Parts: [{ type: "tool_call", ID: "tc-agent-1", Name: "agent", Input: '{"prompt":"investigate"}', Finished: true }, F],
      }),
      // A tool_result for this call marks the delegation "done", which
      // defaults SubAgentBlock closed -- otherwise it's already open while
      // running and the click's force-open would be untestable.
      makeMessage({ ID: "td1", SessionID: "lw-agent-jump", Role: "tool", Parts: [{ type: "tool_result", ToolCallID: "tc-agent-1", Name: "agent", Content: "done", IsError: false }] }),
    ],
  });

  const block = page.locator('[data-tool-call-id="tc-agent-1"]');
  await expect(block.locator(".sub-agent-toggle")).toHaveAttribute("aria-expanded", "false");

  await sendLiveWork(page, "lw-agent-jump", [], [{ toolCallID: "tc-agent-1", toolName: "agent", title: "investigate", startedAt: Date.now() - 1000 }]);
  await page.getByTestId("live-work-tab-agents").click();
  await page.getByTestId("live-work-row-jump").filter({ hasText: "investigate" }).click();

  await expect(block).toBeInViewport();
  await expect(block.locator(".sub-agent-toggle")).toHaveAttribute("aria-expanded", "true");
});

// ── 6. Fallback to Tasks ─────────────────────────────────────────────────────

test("fallback to Tasks when the selected tab's list empties", async ({ page }) => {
  await selectSession(page, "lw-fallback", "LW Fallback");
  await sendLiveWork(page, "lw-fallback", [{ toolCallID: "tc-x", toolName: "bash", title: "echo x", startedAt: Date.now() }]);

  await page.getByTestId("live-work-tab-commands").click();
  await expect(page.getByTestId("live-work-tab-commands")).toHaveAttribute("aria-pressed", "true");

  await sendLiveWork(page, "lw-fallback", []);
  await expect(page.getByTestId("live-work-tab-commands")).toHaveCount(0);
  await expect(page.getByTestId("live-work-tab-tasks")).toHaveAttribute("aria-pressed", "true");
  await expect(page.getByTestId("todo-list")).toBeVisible();
});

// ── 7. Snapshot request on activation ───────────────────────────────────────

test("get_session_live_work is sent when a session becomes active", async ({ page }) => {
  await selectSession(page, "lw-activate", "LW Activate");
  const sent = await waitForWSSend(page, "get_session_live_work");
  expect((sent.payload as { sessionID: string }).sessionID).toBe("lw-activate");
});

// ── 8. Unknown-command error is swallowed, not shown ────────────────────────

test("an unknown-command error reply for get_session_live_work does not show the global error banner", async ({ page }) => {
  await selectSession(page, "lw-error", "LW Error");
  const sent = await waitForWSSend(page, "get_session_live_work");

  await sendMockWSMessage(page, { type: "error", id: sent.id, error: "unknown command: get_session_live_work" });
  await page.waitForTimeout(300);
  await expect(page.locator(".chat-error-banner")).toHaveCount(0);
});

// ── 10. Server-sourced statuses (#1058): distinct terminal causes ───────────

test("terminal statuses render distinctly: running elapsed vs done/failed/timed out/stopped/killed", async ({ page }) => {
  await selectSession(page, "lw-status", "LW Status");
  await sendLiveWork(page, "lw-status", [
    { toolCallID: "tc-s1", toolName: "bash", title: "still going", startedAt: Date.now() - 5000, status: "running" },
    { toolCallID: "tc-s2", toolName: "bash", title: "all good", startedAt: Date.now() - 60000, finishedAt: Date.now() - 30000, lastActivityAt: Date.now() - 30000, status: "completed" },
    { toolCallID: "tc-s3", toolName: "bash", title: "boom", startedAt: Date.now() - 60000, finishedAt: Date.now() - 30000, lastActivityAt: Date.now() - 30000, status: "failed" },
    { toolCallID: "tc-s4", toolName: "bash", title: "too slow", startedAt: Date.now() - 60000, finishedAt: Date.now() - 30000, lastActivityAt: Date.now() - 30000, status: "timed_out", reason: "timeout_terminated" },
    { toolCallID: "tc-s5", toolName: "bash", title: "halting", startedAt: Date.now() - 60000, finishedAt: Date.now() - 30000, lastActivityAt: Date.now() - 30000, status: "cancelled", reason: "session_cancel" },
    { toolCallID: "tc-s6", toolName: "bash", title: "killed", startedAt: Date.now() - 60000, finishedAt: Date.now() - 30000, lastActivityAt: Date.now() - 30000, status: "cancelled", reason: "job_kill" },
    { toolCallID: "tc-s7", toolName: "bash", title: "host died", startedAt: Date.now() - 60000, finishedAt: Date.now() - 30000, lastActivityAt: Date.now() - 30000, status: "interrupted" },
  ]);

  await page.getByTestId("live-work-tab-commands").click();
  const rows = page.getByTestId("live-work-row");

  // A running row shows a ticking elapsed label, not a status word.
  await expect(rows.filter({ hasText: "still going" }).getByTestId("live-work-row-status")).toHaveCount(0);
  await expect(rows.filter({ hasText: "still going" })).toContainText(/\d+s/);
  // Each terminal cause shows its own label.
  await expect(rows.filter({ hasText: "all good" }).getByTestId("live-work-row-status")).toHaveText("done");
  await expect(rows.filter({ hasText: "boom" }).getByTestId("live-work-row-status")).toHaveText("failed");
  await expect(rows.filter({ hasText: "too slow" }).getByTestId("live-work-row-status")).toHaveText("timed out");
  await expect(rows.filter({ hasText: "halting" }).getByTestId("live-work-row-status")).toHaveText("stopped by user");
  await expect(rows.filter({ hasText: "killed" }).getByTestId("live-work-row-status")).toHaveText("killed");
  await expect(rows.filter({ hasText: "host died" }).getByTestId("live-work-row-status")).toHaveText("interrupted");
});

// ── 11. The reply to get_session_live_work populates the tabs ───────────────

test("a get_session_live_work reply (not just a push) populates the Commands tab", async ({ page }) => {
  await selectSession(page, "lw-reply", "LW Reply");
  const sent = await waitForWSSend(page, "get_session_live_work");

  await sendMockWSMessage(page, {
    type: "session_live_work",
    id: sent.id,
    payload: {
      sessionID: "lw-reply",
      commands: [{ toolCallID: "tc-reply", toolName: "run_command", title: "make build", startedAt: Date.now() - 1000, status: "running" }],
      agents: [],
    },
  });

  await expect(page.getByTestId("live-work-tab-commands")).toHaveText("Commands 1");
});

// ── 9. Not-found hint ────────────────────────────────────────────────────────

test("clicking an item with no matching tool call in the transcript shows a hint", async ({ page }) => {
  await selectSession(page, "lw-notfound", "LW Notfound");
  await sendLiveWork(page, "lw-notfound", [{ toolCallID: "tc-missing", toolName: "bash", title: "echo missing", startedAt: Date.now() }]);

  await page.getByTestId("live-work-tab-commands").click();
  await page.getByTestId("live-work-row-jump").filter({ hasText: "echo missing" }).click();

  await expect(page.getByTestId("live-work-row-hint")).toBeVisible();
});
