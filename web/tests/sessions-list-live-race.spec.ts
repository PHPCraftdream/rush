/**
 * Regression coverage for task #690 — sessions_list stale-reply / live-push races (sibling of #685/#689).
 *
 * This spec covers the live-push vs sessions_list-reply race: the request-ordering guard
 * from task #685 only orders REQUESTS; it cannot see session_created/session_updated/session_deleted
 * pushes applied between a list_sessions request's send and its reply. A still-latest reply
 * carrying an older DB snapshot wholesale-replaces $sessions via setSessions (useWS.ts:127) and
 * erases the fresher live state.
 *
 * Concrete failure pattern:
 *   1. Client sends list_sessions (still-latest request per ordering guard)
 *   2. Server reads DB snapshot with N sessions
 *   3. A session_created/session_updated/session_deleted broadcast arrives and is applied
 *   4. The list_sessions reply arrives (still passes staleness guard) carrying only the N-session snapshot
 *   5. setSessions overwrites the list, erasing the fresher live state
 */

import { test, expect, Page } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend, clearWSSent } from "./helpers/mock-ws";
import { makeSession } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
});

const triggerListSessions = (page: Page) =>
  page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));

test("a stale sessions_list reply superseded by a newer request is dropped, not applied", async ({ page }) => {
  await page.clock.install();
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "reorder-a", Title: "ReorderA" })],
  });
  await expect(page.getByTestId("session-title-reorder-a")).toBeVisible({ timeout: 3000 });

  await clearWSSent(page);
  await triggerListSessions(page);
  const req1 = await waitForWSSend(page, "list_sessions", 2000);

  await clearWSSent(page);
  await triggerListSessions(page);
  const req2 = await waitForWSSend(page, "list_sessions", 2000);
  expect(req2).not.toBe(req1);

  // Deliver FRESH reply first
  await sendMockWSMessage(page, {
    type: "sessions_list",
    id: req2.id,
    payload: [
      makeSession({ ID: "reorder-a", Title: "ReorderA" }),
      makeSession({ ID: "reorder-fresh", Title: "ReorderFresh" }),
    ],
  });
  await expect(page.getByTestId("session-title-reorder-fresh")).toBeVisible({ timeout: 2000 });

  // Then deliver STALE reply
  await sendMockWSMessage(page, {
    type: "sessions_list",
    id: req1.id,
    payload: [
      makeSession({ ID: "reorder-a", Title: "ReorderA" }),
      makeSession({ ID: "reorder-stale", Title: "ReorderStale" }),
    ],
  });

  await page.waitForTimeout(300);
  await expect(page.getByTestId("session-title-reorder-stale")).toHaveCount(0);
  await expect(page.getByTestId("session-title-reorder-fresh")).toBeVisible();
  await expect(page.getByTestId("session-title-reorder-a")).toBeVisible();
});

test("a session_created push landing between list_sessions send and its reply is not dropped by the reply", async ({ page }) => {
  await page.clock.install();
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "created-keep", Title: "CreatedKeep" })],
  });
  await expect(page.getByTestId("session-title-created-keep")).toBeVisible({ timeout: 3000 });

  await clearWSSent(page);
  await triggerListSessions(page);
  const req = await waitForWSSend(page, "list_sessions", 2000);

  // Inject live push
  await sendMockWSMessage(page, {
    type: "session_created",
    payload: makeSession({ ID: "created-new", Title: "CreatedNew" }),
  });
  await expect(page.getByTestId("session-title-created-new")).toBeVisible({ timeout: 2000 });

  // Deliver reply whose read predated the create
  await sendMockWSMessage(page, {
    type: "sessions_list",
    id: req.id,
    payload: [makeSession({ ID: "created-keep", Title: "CreatedKeep" })],
  });

  await page.waitForTimeout(300);
  await expect(page.getByTestId("session-title-created-new")).toBeVisible();
  await expect(page.getByTestId("session-title-created-keep")).toBeVisible();
});

test("a session_deleted push is not undone by a sessions_list reply whose read predated the delete", async ({ page }) => {
  await page.clock.install();
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "del-keep", Title: "DelKeep" }),
      makeSession({ ID: "del-victim", Title: "DelVictim" }),
    ],
  });
  await expect(page.getByTestId("session-title-del-keep")).toBeVisible({ timeout: 3000 });
  await expect(page.getByTestId("session-title-del-victim")).toBeVisible();

  await clearWSSent(page);
  await triggerListSessions(page);
  const req = await waitForWSSend(page, "list_sessions", 2000);

  // Inject live push
  await sendMockWSMessage(page, {
    type: "session_deleted",
    payload: { ID: "del-victim" },
  });
  await expect(page.getByTestId("session-title-del-victim")).toHaveCount(0, { timeout: 2000 });

  // Deliver reply that STILL CONTAINS the victim
  await sendMockWSMessage(page, {
    type: "sessions_list",
    id: req.id,
    payload: [
      makeSession({ ID: "del-keep", Title: "DelKeep" }),
      makeSession({ ID: "del-victim", Title: "DelVictim" }),
    ],
  });

  await page.waitForTimeout(300);
  await expect(page.getByTestId("session-title-del-victim")).toHaveCount(0);
  await expect(page.getByTestId("session-title-del-keep")).toBeVisible();
});

test("a fresher session_updated title survives a staler sessions_list reply", async ({ page }) => {
  await page.clock.install();
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "upd-1", Title: "OldTitle" })],
  });
  await expect(page.getByTestId("session-title-upd-1")).toBeVisible({ timeout: 3000 });
  await expect(page.getByTestId("session-title-upd-1")).toHaveText("OldTitle");

  await clearWSSent(page);
  await triggerListSessions(page);
  const req = await waitForWSSend(page, "list_sessions", 2000);

  // Inject live push
  await sendMockWSMessage(page, {
    type: "session_updated",
    payload: makeSession({ ID: "upd-1", Title: "NewTitle" }),
  });
  await expect(page.getByTestId("session-title-upd-1")).toHaveText("NewTitle", { timeout: 2000 });

  // Deliver reply with the stale title
  await sendMockWSMessage(page, {
    type: "sessions_list",
    id: req.id,
    payload: [makeSession({ ID: "upd-1", Title: "OldTitle" })],
  });

  await page.waitForTimeout(300);
  await expect(page.getByTestId("session-title-upd-1")).toHaveText("NewTitle");
});

test("CONTROL: an epoch-clean sessions_list reply still wholesale-replaces the list", async ({ page }) => {
  await page.clock.install();
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "ctrl-keep", Title: "CtrlKeep" }),
      makeSession({ ID: "ctrl-vanish", Title: "CtrlVanish" }),
    ],
  });
  await expect(page.getByTestId("session-title-ctrl-keep")).toBeVisible({ timeout: 3000 });
  await expect(page.getByTestId("session-title-ctrl-vanish")).toBeVisible();

  await clearWSSent(page);
  await triggerListSessions(page);
  const req = await waitForWSSend(page, "list_sessions", 2000);

  // NO live pushes injected — this is the control case

  // Deliver reply WITHOUT ctrl-vanish
  await sendMockWSMessage(page, {
    type: "sessions_list",
    id: req.id,
    payload: [makeSession({ ID: "ctrl-keep", Title: "CtrlKeep" })],
  });

  await page.waitForTimeout(300);
  await expect(page.getByTestId("session-title-ctrl-vanish")).toHaveCount(0);
  await expect(page.getByTestId("session-title-ctrl-keep")).toBeVisible();
});
