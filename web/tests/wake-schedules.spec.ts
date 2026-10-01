/**
 * Wake schedules panel + wake-notice rendering (stage 5b, #1107).
 *
 * Drives the client entirely off synthetic WS events via the mock-WS
 * fixtures (same style as live-work-panel.spec.ts / async-job-status.spec.ts):
 * `session_wake_schedules` snapshots for the Schedules tab, and
 * `messages_list` payloads carrying wake/timeout notice messages.
 *
 * Verifies:
 *  1. The Schedules tab is hidden while the snapshot is empty and appears
 *     with the row count once schedules exist.
 *  2. A schedule row shows kind, message, recurrence and next-fire time.
 *  3. get_session_wake_schedules is sent when a session becomes active.
 *  4. Cancel: the button opens the shared ConfirmDialog; dismissing sends
 *     nothing; confirming sends cancel_wake_schedule(sessionID, scheduleID),
 *     and the fresh snapshot that removes the row also removes the tab.
 *  5. A wake_fired notice renders as a SYSTEM event (the "wake" chip),
 *     never as a human-typed user bubble.
 *  6. Timed-out / user-stopped async commands get distinct badges on their
 *     tool-call block (not a generic "error").
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend, type MockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeMessage, makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
});

async function selectSession(page: import("@playwright/test").Page, id: string, _title: string) {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [makeSession({ ID: id, Title: _title })] });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  // Flake fix: click the row by its stable test id, never by title text.
  // Text matching raced the sidebar's re-render (the config snapshot
  // arriving right after sessions_list recreates every row node), so the
  // element matched by `getByText(title)` could be detached between
  // toBeVisible and the click. The test id is keyed by the session ID,
  // which never changes, and Playwright re-resolves the locator on a
  // mid-click detach.
  const row = page.getByTestId(`session-${id}`);
  await expect(row).toBeVisible({ timeout: 5000 });
  await row.click();
}

function schedule(overrides: Record<string, unknown> = {}) {
  return {
    id: "wake_1",
    kind: "once",
    message: "check the nightly build",
    nextRunAt: Date.now() + 60_000,
    everyMs: 0,
    maxRuns: 0,
    untilAt: 0,
    state: "active",
    occurrence: 0,
    createdAt: Date.now(),
    ...overrides,
  };
}

// ── 1/2. Tab visibility + row content ────────────────────────────────────────

test("Schedules tab hidden while empty, appears with snapshot and shows row details", async ({ page }) => {
  await selectSession(page, "ws-empty", "WS Empty");
  await expect(page.getByTestId("live-work-tab-schedules")).toHaveCount(0);

  await sendMockWSMessage(page, {
    type: "session_wake_schedules",
    payload: {
      sessionID: "ws-empty",
      schedules: [
        schedule({ id: "wake_once", kind: "once", message: "check the nightly build" }),
        schedule({
          id: "wake_loop", kind: "loop", message: "poll the deploy",
          everyMs: 300_000, maxRuns: 5, occurrence: 2,
        }),
      ],
    },
  });

  await expect(page.getByTestId("live-work-tab-schedules")).toHaveText("Schedules 2");
  await page.getByTestId("live-work-tab-schedules").click();
  const rows = page.getByTestId("wake-schedule-row");
  await expect(rows).toHaveCount(2);
  const onceRow = page.locator('[data-test-id="wake-schedule-row"]').filter({ hasText: "check the nightly build" });
  await expect(onceRow.getByTestId("wake-schedule-kind")).toHaveText("wake");
  await expect(onceRow).toContainText("one-time");
  const loopRow = page.locator('[data-test-id="wake-schedule-row"]').filter({ hasText: "poll the deploy" });
  await expect(loopRow.getByTestId("wake-schedule-kind")).toHaveText("loop");
  await expect(loopRow.getByTestId("wake-schedule-detail")).toContainText("every 300s");
  await expect(loopRow.getByTestId("wake-schedule-detail")).toContainText("2/5");
});

// ── 3. Activation request ────────────────────────────────────────────────────

test("activating a session requests a wake-schedule snapshot", async ({ page }) => {
  await selectSession(page, "ws-req", "WS Request");
  const sent = await waitForWSSend(page, "get_session_wake_schedules");
  expect((sent.payload as { sessionID?: string }).sessionID).toBe("ws-req");
});

// ── 4. Cancel with confirmation ──────────────────────────────────────────────

test("cancel asks for confirmation, sends the command, and the fresh snapshot removes the row", async ({ page }) => {
  await selectSession(page, "ws-cancel", "WS Cancel");
  await sendMockWSMessage(page, {
    type: "session_wake_schedules",
    payload: { sessionID: "ws-cancel", schedules: [schedule({ id: "wake_kill" })] },
  });
  await page.getByTestId("live-work-tab-schedules").click();

  await page.getByTestId("wake-schedule-cancel").click();
  await expect(page.getByTestId("confirm-dialog")).toBeVisible();

  // Dismissing must NOT send anything.
  await page.getByTestId("confirm-dialog").getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(page.getByTestId("confirm-dialog")).toHaveCount(0);
  const notSent = await page.evaluate(
    () => (((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>)
      .filter((m) => m.type === "cancel_wake_schedule").length,
  );
  expect(notSent).toBe(0);

  // Confirming sends cancel_wake_schedule with the row's ids...
  await page.getByTestId("wake-schedule-cancel").click();
  await page.getByTestId("confirm-dialog").getByRole("button", { name: "Cancel schedule" }).click();
  const sent = await waitForWSSend(page, "cancel_wake_schedule");
  const payload = sent.payload as { sessionID: string; scheduleID: string };
  expect(payload.sessionID).toBe("ws-cancel");
  expect(payload.scheduleID).toBe("wake_kill");

  // ...and the server's fresh snapshot removes the row -- and with it the tab.
  await sendMockWSMessage(page, {
    type: "session_wake_schedules",
    payload: { sessionID: "ws-cancel", schedules: [] },
  });
  await expect(page.getByTestId("live-work-tab-schedules")).toHaveCount(0);
  await expect(page.getByTestId("wake-schedule-row")).toHaveCount(0);
});

// ── 5. Wake notice renders as a system event ────────────────────────────────

test("a wake_fired notice renders as a system event, not a user bubble", async ({ page }) => {
  await selectSession(page, "ws-notice", "WS Notice");
  await sendMockWSMessage(page, { type: "messages_list", payload: { SessionID: "ws-notice", Messages: [
    makeMessage({
      ID: "msg-wake",
      SessionID: "ws-notice",
      Role: "user",
      BackgroundJobNotice: true,
      NoticeKind: "wake_fired",
      Parts: [{ type: "text", Text: "Wake wake_1 fired (scheduled for 2026-10-01T02:00:00Z): check the build" }],
    }),
  ] } });

  await expect(page.getByTestId("notice-kind-label")).toHaveText(/wake/);
  // The notice's text is collapsed behind the system spoiler, never shown
  // as a user-typed bubble.
  await expect(page.getByText("check the build")).toHaveCount(0);
});

test("a reaction_chain marker renders as a system event", async ({ page }) => {
  await selectSession(page, "ws-chain", "WS Chain");
  await sendMockWSMessage(page, { type: "messages_list", payload: { SessionID: "ws-chain", Messages: [
    makeMessage({
      ID: "msg-chain",
      SessionID: "ws-chain",
      Role: "user",
      BackgroundJobNotice: true,
      NoticeKind: "reaction_chain",
      Parts: [{ type: "text", Text: "Reaction chain stopped: 3 automatic turns only ran sleep/echo" }],
    }),
  ] } });

  await expect(page.getByTestId("notice-kind-label")).toHaveText(/reaction chain stopped/);
  await expect(page.getByText("only ran sleep/echo")).toHaveCount(0);
});

// ── 6. Distinct terminal badges on async tool-call blocks ───────────────────

test("timed out and user-stopped async commands get distinct badges", async ({ page }) => {
  await selectSession(page, "ws-badges", "WS Badges");
  const messages = [
    makeMessage({ ID: "user-b1", SessionID: "ws-badges", Role: "user", Parts: [{ type: "text", Text: "long job" }] }),
    makeMessage({ ID: "assistant-b1", SessionID: "ws-badges", Role: "assistant", Parts: [
      { type: "tool_call", ID: "call-timeout", Name: "bash", Input: '{"command":"sleep 100"}', Finished: true },
      { type: "tool_call", ID: "call-stop", Name: "run_command", Input: '{"program":"tail","args":["-f","x"]}', Finished: true },
    ] }),
    makeMessage({ ID: "result-b1", SessionID: "ws-badges", Role: "tool", Parts: [
      { type: "tool_result", ToolCallID: "call-timeout", Name: "bash", Content: "Async job started", IsError: false, Metadata: '{"async":true,"job_id":"call-timeout","status":"running"}' },
      { type: "tool_result", ToolCallID: "call-stop", Name: "run_command", Content: "Async job started", IsError: false, Metadata: '{"async":true,"job_id":"call-stop","status":"running"}' },
    ] }),
  ];
  await sendMockWSMessage(page, { type: "messages_list", payload: { SessionID: "ws-badges", Messages: messages } });

  await sendMockWSMessage(page, { type: "messages_list", payload: { SessionID: "ws-badges", Messages: [
    ...messages,
    makeMessage({ ID: "notice-timeout", SessionID: "ws-badges", Role: "user", NoticeKind: "timeout_terminated", Parts: [
      { type: "text", Text: "Async job call-timeout (bash) timed out after 60s and was stopped. Partial output:\n\n(no output)" },
    ] }),
    makeMessage({ ID: "notice-stop", SessionID: "ws-badges", Role: "user", BackgroundJobNotice: true, Parts: [
      { type: "text", Text: "Async job call-stop (run_command) was stopped (job_kill). Partial output before the stop:\n\nline one" },
    ] }),
  ] } });

  const timeoutRow = page.locator('[data-test-id="action-row"]').filter({ hasText: "sleep 100" });
  await expect(timeoutRow.getByTestId("action-row-status")).toHaveText("timed out");
  const stopRow = page.locator('[data-test-id="action-row"]').filter({ hasText: "run_command" });
  await expect(stopRow.getByTestId("action-row-status")).toHaveText("stopped by user");
  await expect(timeoutRow.getByText("error")).toHaveCount(0);
});

// ── 7. Rejected cancel: inline error in the dialog ───────────────────────────

test("a rejected cancel surfaces the server error inline in the dialog", async ({ page }) => {
  await selectSession(page, "ws-cancel-err", "WS Cancel Err");
  await sendMockWSMessage(page, {
    type: "session_wake_schedules",
    payload: { sessionID: "ws-cancel-err", schedules: [schedule({ id: "wake_ghost" })] },
  });
  await page.getByTestId("live-work-tab-schedules").click();

  await page.getByTestId("wake-schedule-cancel").click();
  await page.getByTestId("confirm-dialog").getByRole("button", { name: "Cancel schedule" }).click();
  const sent = await waitForWSSend(page, "cancel_wake_schedule");

  // The server refuses (unknown scheduleID) and echoes the correlation id
  // of the cancel request; the dialog must stay open and show the error
  // INLINE, not in the global banner.
  await sendMockWSMessage(page, {
    type: "error",
    payload: null,
    id: sent.id,
    error: "wake schedule not found",
  } as unknown as MockWSMessage);
  await expect(page.getByTestId("confirm-dialog")).toBeVisible();
  await expect(page.getByTestId("confirm-dialog-error")).toHaveText("wake schedule not found");
  await expect(page.locator(".chat-error-banner")).toHaveCount(0);
});
