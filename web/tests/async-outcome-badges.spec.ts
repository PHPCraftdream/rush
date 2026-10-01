/**
 * Async outcome badges — command and delegation matrices (stage 5b
 * follow-up): EVERY terminal cause the contract's §5.1 texts carry must get
 * its own badge on the tool-call block, for both async commands
 * (bash/run_command → ActionRow) and async delegations (agent →
 * SubAgentBlock).
 *
 * Outcome texts are the exact FormatAsyncCompletion outputs (internal/agent/
 * coordinator_background.go), plus stop_agent's delivery shape for a
 * user-stopped delegation: a plain FAILED completion whose entire content
 * is subAgentOutcomeCancelledText (work_ledger_delegation.go) — mapped to
 * "stopped by user", NOT "error".
 *
 * Revert-check: dropping asyncStoppedPattern (or its "before the stop"
 * variant), the delegation-cancelled mapping, or any of the badge branches
 * in ActionRow.tsx / SubAgentBlock.tsx fails the matching assertion below
 * (the badge element disappears or reads as "error"/"done").
 */

import { expect, test } from "@playwright/test";
import { makeMessage, makeSession } from "./helpers/fixtures";
import { sendMockWSMessage, setupMockWS } from "./helpers/mock-ws";

const sessionID = "outcome-badges";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
});

async function selectSession(page: import("@playwright/test").Page, id: string) {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [makeSession({ ID: id, Title: id })] });
  const row = page.getByTestId(`session-${id}`);
  await expect(row).toBeVisible({ timeout: 5000 });
  await row.click();
}

// ── Command matrix (ActionRow badges) ────────────────────────────────────────

test("every terminal cause gets a distinct badge on an async command block", async ({ page }) => {
  await selectSession(page, sessionID);

  const calls: Array<{ id: string; notice: string; badge: string }> = [
    { id: "c-fin", notice: "Async job c-fin (bash) finished.\n\nok out", badge: "done" },
    { id: "c-fail", notice: "Async job c-fail (bash) failed.\n\nboom", badge: "error" },
    { id: "c-to", notice: "Async job c-to (bash) timed out after 60s and was stopped. Partial output:\n\npartial", badge: "timed out" },
    { id: "c-stop", notice: "Async job c-stop (bash) was stopped (job_kill). Partial output before the stop:\n\npartial", badge: "stopped by user" },
    { id: "c-can", notice: "Async job c-can (run_command) was cancelled (session stopped). Partial output:\n\npartial", badge: "cancelled" },
  ];

  const messages = [
    makeMessage({ ID: "user-c", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "run jobs" }] }),
    makeMessage({
      ID: "assistant-c", SessionID: sessionID, Role: "assistant",
      Parts: calls.map((c) => ({ type: "tool_call", ID: c.id, Name: "bash", Input: `{"command":"echo ${c.id}"}`, Finished: true })),
    }),
    makeMessage({
      ID: "results-c", SessionID: sessionID, Role: "tool",
      Parts: calls.map((c) => ({ type: "tool_result", ToolCallID: c.id, Name: "bash", Content: "Async job started", IsError: false, Metadata: `{"async":true,"job_id":"${c.id}","status":"running"}` })),
    }),
  ];
  await sendMockWSMessage(page, { type: "messages_list", payload: { SessionID: sessionID, Messages: messages } });

  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: {
      SessionID: sessionID,
      Messages: [
        ...messages,
        ...calls.map((c, i) => makeMessage({
          ID: `notice-c-${i}`, SessionID: sessionID, Role: "user" as const,
          BackgroundJobNotice: c.badge !== "timed out",
          NoticeKind: c.badge === "timed out" ? "timeout_terminated" : undefined,
          Parts: [{ type: "text", Text: c.notice }],
        })),
      ],
    },
  });

  await Promise.all(calls.map(async (c) => {
    const row = page.locator('[data-test-id="action-row"]').filter({ hasText: `echo ${c.id}` });
    if (c.badge === "done" || c.badge === "error") {
      await expect(row.getByText(c.badge, { exact: true })).toBeVisible();
    } else {
      await expect(row.getByTestId("action-row-status")).toHaveText(c.badge);
    }
  }));
});

// ── Delegation matrix (SubAgentBlock badges) ─────────────────────────────────

test("every terminal cause gets a distinct badge on an async delegation block", async ({ page }) => {
  await selectSession(page, sessionID);

  const calls: Array<{ id: string; prompt: string; notice: string; badge: string }> = [
    { id: "a-fin", prompt: "delegate finish", notice: "Async job a-fin (agent) finished.\n\nchild report", badge: "done" },
    { id: "a-fail", prompt: "delegate fail", notice: "Async job a-fail (agent) failed.\n\nchild blew up", badge: "error" },
    { id: "a-to", prompt: "delegate timeout", notice: "Async job a-to (agent) timed out after 90s and was stopped. Partial output:\n\npartial child", badge: "timed out" },
    // stop_agent's parent delivery: a FAILED completion whose entire content
    // is subAgentOutcomeCancelledText — must read "stopped by user", not
    // "error".
    { id: "a-stop", prompt: "delegate stop", notice: "Async job a-stop (agent) failed.\n\nsub-agent canceled", badge: "stopped by user" },
    { id: "a-can", prompt: "delegate cancel", notice: "Async job a-can (agent) was cancelled (session stopped). Partial output:\n\npartial", badge: "cancelled" },
  ];

  const messages = [
    makeMessage({ ID: "user-a", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "delegate work" }] }),
    makeMessage({
      ID: "assistant-a", SessionID: sessionID, Role: "assistant",
      Parts: calls.map((c) => ({ type: "tool_call", ID: c.id, Name: "agent", Input: `{"prompt":"${c.prompt}"}`, Finished: true })),
    }),
    makeMessage({
      ID: "results-a", SessionID: sessionID, Role: "tool",
      Parts: calls.map((c) => ({ type: "tool_result", ToolCallID: c.id, Name: "agent", Content: "Async agent job started", IsError: false, Metadata: `{"async":true,"job_id":"${c.id}","status":"running"}` })),
    }),
  ];
  await sendMockWSMessage(page, { type: "messages_list", payload: { SessionID: sessionID, Messages: messages } });

  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: {
      SessionID: sessionID,
      Messages: [
        ...messages,
        ...calls.map((c, i) => makeMessage({
          ID: `notice-a-${i}`, SessionID: sessionID, Role: "user" as const,
          BackgroundJobNotice: c.badge !== "timed out",
          NoticeKind: c.badge === "timed out" ? "timeout_terminated" : undefined,
          Parts: [{ type: "text", Text: c.notice }],
        })),
      ],
    },
  });

  await Promise.all(calls.map(async (c) => {
    const block = page.locator(".sub-agent-block").filter({ hasText: c.prompt });
    await expect(block.locator(".sub-agent-toggle").getByText("running...", { exact: true })).toHaveCount(0);
    if (c.badge === "done" || c.badge === "error") {
      await expect(block.locator(".sub-agent-toggle").getByText(c.badge, { exact: true })).toBeVisible();
    } else {
      await expect(block.getByTestId("sub-agent-status")).toHaveText(c.badge);
    }
  }));
});
