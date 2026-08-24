/**
 * Sidebar session delete round-trip tests (task #684, F-1 of
 * docs/reviews/2026-08-24-twenty-seventh-review-beed48a4.md).
 *
 * confirmDelete()/confirmDeleteOtherSessions() used to remove the row (or
 * rows) from $sessions optimistically, in the same tick as ws.send, with no
 * msgID and no reply listener — the same fire-and-forget shape task #683
 * fixed in SystemPromptModal.save(), one layer removed (a list-backed store
 * row removal instead of a local dirty-flag reset). A rejected
 * delete_session (EventError, e.g. "database is locked" from
 * handlers_sessions.go's handleDeleteSession) made the row vanish
 * immediately and only self-healed via the next ≤5s sessions_list poll.
 * delete_other_sessions was worse: a per-session failure inside its loop
 * was only slog.Warn'd server-side while the handler still replied an
 * unqualified EventResponse{"status":"ok"} — the client had no way to tell
 * partial failure from full success.
 *
 * Fix: both functions now generate a msgID and register a one-shot
 * ws.on("*", ...) reply listener (mirroring MCPForm.submit() /
 * SystemPromptModal.save()), only mutating $sessions on a genuine reply.
 * delete_other_sessions's server reply shape changed from a bare
 * {"status":"ok"} to {deletedIDs, failedIDs} (protocol.go's
 * DeleteOtherSessionsResult) so the client only removes rows the server
 * actually confirms deleted. ConfirmDialog gained optional error/busy
 * props so a rejected delete shows its error inline in the still-open
 * dialog instead of relying solely on the transcript-pane banner.
 *
 * Covers:
 *  - CONTROL: a successful delete_session removes the row and it stays removed
 *  - a rejected delete_session leaves the row in place with a visible inline error
 *  - delete_other_sessions only removes rows the server confirms deleted on a
 *    partial failure (not everything, not trusting an unqualified "ok")
 *  - delete_other_sessions full success removes every non-kept row and closes the dialog
 *  - a rejected delete of the operator's only session does not cascade into a
 *    spurious create_session
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend, clearWSSent } from "./helpers/mock-ws";
import { makeSession } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
});

test("CONTROL: successful delete_session removes the row and stays removed", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "sd-ctrl-1", Title: "Keep Me" }),
      makeSession({ ID: "sd-ctrl-2", Title: "Delete Me" }),
    ],
  });
  await expect(page.getByTestId("session-title-sd-ctrl-2")).toBeVisible({ timeout: 3000 });

  const row = page.getByTestId("session-sd-ctrl-2");
  await row.hover();
  await page.getByTestId("session-delete-sd-ctrl-2").click();
  await page.getByText("Delete", { exact: true }).click();

  const cmd = await waitForWSSend(page, "delete_session");
  expect((cmd.payload as { sessionID: string }).sessionID).toBe("sd-ctrl-2");
  expect(cmd.id, "delete_session must carry a msgID so the client can await its reply").toBeTruthy();

  await sendMockWSMessage(page, {
    type: "response",
    id: cmd.id,
    payload: { status: "ok" },
  });

  await expect(page.getByTestId("session-sd-ctrl-2")).not.toBeVisible({ timeout: 2000 });
  await page.waitForTimeout(500);
  await expect(page.getByTestId("session-sd-ctrl-2")).not.toBeVisible();
});

test("a rejected delete_session leaves the row in place with a visible inline error", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "sd-bug-1", Title: "Keep Me" }),
      makeSession({ ID: "sd-bug-2", Title: "Reject Me" }),
    ],
  });
  await expect(page.getByTestId("session-title-sd-bug-2")).toBeVisible({ timeout: 3000 });

  const row = page.getByTestId("session-sd-bug-2");
  await row.hover();
  await page.getByTestId("session-delete-sd-bug-2").click();
  await page.getByText("Delete", { exact: true }).click();

  const cmd = await waitForWSSend(page, "delete_session");

  // Exact wire shape from handlers_sessions.go's handleDeleteSession on failure.
  await sendMockWSMessage(page, {
    type: "error",
    id: cmd.id,
    error: "database is locked",
  });

  // The row must not vanish before the server confirms the delete.
  await page.waitForTimeout(300);
  await expect(page.getByTestId("session-sd-bug-2")).toBeVisible();

  // The error is visible inline in the still-open confirm dialog, not only
  // via the global transcript-pane banner (task #683's inadequate-signal note).
  await expect(page.getByTestId("confirm-dialog-error")).toHaveText("database is locked", { timeout: 2000 });
  await expect(page.getByText("Delete session")).toBeVisible();
});

test("delete_other_sessions only removes rows the server confirms deleted, reflecting a partial failure", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "sd-multi-active", Title: "Active" }),
      makeSession({ ID: "sd-multi-a", Title: "Deletes OK" }),
      makeSession({ ID: "sd-multi-b", Title: "Fails Server-Side" }),
    ],
  });
  await expect(page.getByTestId("session-title-sd-multi-active")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("session-sd-multi-active").click();

  await clearWSSent(page);
  await page.getByTestId("sidebar-delete-others").click();
  await page.getByText("Delete all", { exact: true }).click();

  const cmd = await waitForWSSend(page, "delete_other_sessions");

  // New reply shape (protocol.go's DeleteOtherSessionsResult): "a" genuinely
  // deleted, "b" failed server-side — no more blanket {"status":"ok"}.
  await sendMockWSMessage(page, {
    type: "response",
    id: cmd.id,
    payload: { deletedIDs: ["sd-multi-a"], failedIDs: ["sd-multi-b"] },
  });

  await expect(page.getByTestId("session-sd-multi-a")).not.toBeVisible({ timeout: 2000 });
  // The row the server reports as failed must remain visible.
  await expect(page.getByTestId("session-sd-multi-b")).toBeVisible();

  // Partial failure surfaces inline instead of silently vanishing.
  await expect(page.getByTestId("confirm-dialog-error")).toBeVisible({ timeout: 2000 });
});

test("delete_other_sessions full success removes every non-kept row and closes the dialog", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "sd-full-active", Title: "Active" }),
      makeSession({ ID: "sd-full-a", Title: "A" }),
      makeSession({ ID: "sd-full-b", Title: "B" }),
    ],
  });
  await expect(page.getByTestId("session-title-sd-full-active")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("session-sd-full-active").click();

  await clearWSSent(page);
  await page.getByTestId("sidebar-delete-others").click();
  await page.getByText("Delete all", { exact: true }).click();

  const cmd = await waitForWSSend(page, "delete_other_sessions");
  await sendMockWSMessage(page, {
    type: "response",
    id: cmd.id,
    payload: { deletedIDs: ["sd-full-a", "sd-full-b"], failedIDs: [] },
  });

  await expect(page.getByTestId("session-sd-full-a")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("session-sd-full-b")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Delete all other sessions")).not.toBeVisible({ timeout: 2000 });
});

// ── Edge case (ported from round 27's own follow-up check): deleting the
// operator's only session and having that delete rejected must not cascade
// into a spurious create_session. ────────────────────────────────────────

test("a rejected delete of the operator's only session does not spuriously create_session", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sd-only", Title: "Only Session" })],
  });
  await expect(page.getByTestId("session-title-sd-only")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("session-sd-only").click();

  await clearWSSent(page);
  const row = page.getByTestId("session-sd-only");
  await row.hover();
  await page.getByTestId("session-delete-sd-only").click();
  await page.getByText("Delete", { exact: true }).click();

  const cmd = await waitForWSSend(page, "delete_session");
  await sendMockWSMessage(page, {
    type: "error",
    id: cmd.id,
    error: "database is locked",
  });

  await expect(page.getByTestId("session-sd-only")).toBeVisible({ timeout: 2000 });

  await page.waitForTimeout(500);
  const sent = await page.evaluate(() => {
    const arr = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }> | null;
    return (arr ?? []).filter((m) => m.type === "create_session");
  });
  expect(sent, "a rejected delete of the only session must not trigger a spurious create_session").toEqual([]);
});
