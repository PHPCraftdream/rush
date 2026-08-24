/**
 * Sidebar delete reply stale-closure tests (task #694, F6 of the external
 * readonly review).
 *
 * confirmDelete()/confirmDeleteOtherSessions() (task #684's msgID + one-shot
 * reply listener pattern) closed over the `activeID`/`allSessions` values
 * captured by the render that INITIATED the delete. The active session can
 * legitimately change while the delete reply is still in flight — the
 * confirm dialog's full-screen overlay blocks sidebar row clicks for the
 * whole window, but live events are not pointer-driven and still switch it:
 *   - session_created auto-focuses the new session (useWS.ts),
 *   - hashchange (browser back/forward) re-activates the hashed session,
 *   - sessions_list hash routing re-syncs the active session.
 *
 * When the reply finally arrived, the stale `if (activeID === target.id)`
 * compared the OLD active: deleting X while X was active at request time
 * and switching to Y mid-flight cleared Y (empty hash, wiped transcript)
 * even though Y was never deleted; switching TO the deleted session
 * mid-flight left a dangling active pointing at a session the server had
 * just removed. delete_other_sessions had the same shape twice over: the
 * row filter walked the request-time allSessions (a session created
 * mid-flight and deleted server-side survived as a ghost row) and never
 * noticed the active session had been switched onto a deleted ID.
 *
 * Fix: the reply handlers re-read $activeSessionID/$sessions at reply time
 * and only clear/remove based on what the server's reply actually confirms,
 * mirroring useWS's session_deleted handler. These tests deliberately never
 * deliver session_deleted broadcasts alongside the replies — the reply path
 * alone must be correct (broadcast vs reply ordering is not guaranteed
 * server-side, so self-healing must not be load-bearing).
 *
 * Covers:
 *  - BUG: active switched away (session_created) mid-delete stays active
 *    with its transcript intact after the reply
 *  - BUG: active switched TO the deleted session (hashchange) mid-delete is
 *    cleared by the reply instead of left dangling
 *  - BUG: delete_other_sessions removes a mid-flight-created row the server
 *    confirms deleted and clears an active switched onto a deleted ID
 *  - CONTROL: deleting the still-active session still clears the active
 *  - CONTROL: delete_other_sessions keeps the keep session active
 */

import { test, expect, type Page } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
});

async function currentHash(page: Page): Promise<string> {
  return page.evaluate(() => window.location.hash);
}

test("BUG: active session switched away mid-delete survives the delete_session reply", async ({ page }) => {
  await page.goto("/");
  // First list entry becomes the auto-picked "most recent" active session.
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "race-away-x", Title: "Doomed Active" }),
      makeSession({ ID: "race-away-other", Title: "Bystander" }),
    ],
  });
  await expect(page.getByTestId("session-title-race-away-x")).toBeVisible({ timeout: 3000 });
  expect(await currentHash(page)).toBe("#/race-away-x");

  // Start deleting the currently-ACTIVE session; the reply is held back.
  const row = page.getByTestId("session-race-away-x");
  await row.hover();
  await page.getByTestId("session-delete-race-away-x").click();
  await page.getByText("Delete", { exact: true }).click();
  const cmd = await waitForWSSend(page, "delete_session");
  expect((cmd.payload as { sessionID: string }).sessionID).toBe("race-away-x");
  expect(cmd.id).toBeTruthy();

  // Mid-flight: a live session_created auto-switches the active session.
  // The dialog's overlay blocks clicks, but not this.
  await sendMockWSMessage(page, {
    type: "session_created",
    payload: makeSession({ ID: "race-away-new", Title: "Fresh Switch" }),
  });
  await expect(page.getByTestId("session-title-race-away-new")).toBeVisible({ timeout: 2000 });
  expect(await currentHash(page)).toBe("#/race-away-new");

  // Load the new active session's transcript so its content is on screen.
  const load = await waitForWSSend(page, "load_messages");
  expect((load.payload as { sessionID: string }).sessionID).toBe("race-away-new");
  await sendMockWSMessage(page, {
    type: "messages_list",
    id: load.id,
    payload: {
      SessionID: "race-away-new",
      Messages: [
        makeMessage({
          ID: "m-race-away-new",
          SessionID: "race-away-new",
          Parts: [{ type: "text", Text: "fresh transcript intact" }],
        }),
      ],
    },
  });
  await expect(page.getByText("fresh transcript intact")).toBeVisible({ timeout: 2000 });

  // The delete reply lands AFTER the switch.
  await sendMockWSMessage(page, { type: "response", id: cmd.id, payload: { status: "ok" } });

  // The deleted row is gone…
  await expect(page.getByTestId("session-race-away-x")).not.toBeVisible({ timeout: 2000 });
  // …but the session that is active NOW is untouched: hash intact, no
  // transcript wipe (stale code cleared the active via the request-time
  // capture and reloaded nothing but an empty pane).
  expect(await currentHash(page)).toBe("#/race-away-new");
  await expect(page.getByText("fresh transcript intact")).toBeVisible();
});

test("BUG: active session switched ONTO the deleted session mid-delete is cleared by the reply", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "race-onto-y", Title: "Stays Active" }),
      makeSession({ ID: "race-onto-x", Title: "Doomed Idle" }),
    ],
  });
  await expect(page.getByTestId("session-title-race-onto-y")).toBeVisible({ timeout: 3000 });
  expect(await currentHash(page)).toBe("#/race-onto-y");

  // Start deleting the NON-active session; hold the reply. The stale
  // capture is race-onto-y, so old code took the "not active" branch.
  const row = page.getByTestId("session-race-onto-x");
  await row.hover();
  await page.getByTestId("session-delete-race-onto-x").click();
  await page.getByText("Delete", { exact: true }).click();
  const cmd = await waitForWSSend(page, "delete_session");

  // Mid-flight: browser history navigation re-activates the doomed session
  // via hashchange (the overlay blocks clicks, not hash changes).
  await page.evaluate(() => {
    window.location.hash = "#/race-onto-x";
  });
  const load = await waitForWSSend(page, "load_messages");
  expect((load.payload as { sessionID: string }).sessionID).toBe("race-onto-x");

  // Reply lands while the doomed session IS the active one.
  await sendMockWSMessage(page, { type: "response", id: cmd.id, payload: { status: "ok" } });

  await expect(page.getByTestId("session-race-onto-x")).not.toBeVisible({ timeout: 2000 });
  // The active session pointed at a session the server just deleted — the
  // reply must notice and clear it (stale code left it dangling).
  expect(await currentHash(page)).toBe("");
});

test("BUG: delete_other_sessions reconciles rows and active session against current state, not request-time state", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "race-others-k", Title: "Keeper" }),
      makeSession({ ID: "race-others-a", Title: "Old Casualty" }),
    ],
  });
  await expect(page.getByTestId("session-title-race-others-k")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("session-race-others-k").click();
  expect(await currentHash(page)).toBe("#/race-others-k");

  await page.getByTestId("sidebar-delete-others").click();
  await page.getByText("Delete all", { exact: true }).click();
  const cmd = await waitForWSSend(page, "delete_other_sessions");
  expect((cmd.payload as { keepID: string }).keepID).toBe("race-others-k");
  expect(cmd.id).toBeTruthy();

  // Mid-flight: a live session_created both adds a row and auto-switches
  // the active session onto it. The server processed the delete AFTER the
  // create, so the reply confirms the new session deleted too.
  await sendMockWSMessage(page, {
    type: "session_created",
    payload: makeSession({ ID: "race-others-z", Title: "Mid-Flight Create" }),
  });
  await expect(page.getByTestId("session-title-race-others-z")).toBeVisible({ timeout: 2000 });

  await sendMockWSMessage(page, {
    type: "response",
    id: cmd.id,
    payload: { deletedIDs: ["race-others-a", "race-others-z"], failedIDs: [] },
  });

  // Both server-confirmed rows are gone — including the one created after
  // the request was sent (stale code never saw it in its captured list and
  // left a ghost row).
  await expect(page.getByTestId("session-race-others-a")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("session-race-others-z")).not.toBeVisible({ timeout: 2000 });
  // The active session was switched onto a session the server deleted — it
  // must be cleared, not left dangling (mirrors session_deleted handling).
  expect(await currentHash(page)).toBe("");
  // The keeper survives.
  await expect(page.getByTestId("session-race-others-k")).toBeVisible();
});

test("CONTROL: deleting the still-active session still clears the active session", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "race-ctrl-x", Title: "Doomed Active" }),
      makeSession({ ID: "race-ctrl-y", Title: "Bystander" }),
    ],
  });
  await expect(page.getByTestId("session-title-race-ctrl-x")).toBeVisible({ timeout: 3000 });
  expect(await currentHash(page)).toBe("#/race-ctrl-x");

  const row = page.getByTestId("session-race-ctrl-x");
  await row.hover();
  await page.getByTestId("session-delete-race-ctrl-x").click();
  await page.getByText("Delete", { exact: true }).click();
  const cmd = await waitForWSSend(page, "delete_session");

  await sendMockWSMessage(page, { type: "response", id: cmd.id, payload: { status: "ok" } });

  await expect(page.getByTestId("session-race-ctrl-x")).not.toBeVisible({ timeout: 2000 });
  expect(await currentHash(page)).toBe("");
  await expect(page.getByTestId("session-race-ctrl-y")).toBeVisible();
});

test("CONTROL: delete_other_sessions keeps the keep session active", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "race-ctrl-k", Title: "Keeper" }),
      makeSession({ ID: "race-ctrl-a", Title: "A" }),
      makeSession({ ID: "race-ctrl-b", Title: "B" }),
    ],
  });
  await expect(page.getByTestId("session-title-race-ctrl-k")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("session-race-ctrl-k").click();
  expect(await currentHash(page)).toBe("#/race-ctrl-k");

  await page.getByTestId("sidebar-delete-others").click();
  await page.getByText("Delete all", { exact: true }).click();
  const cmd = await waitForWSSend(page, "delete_other_sessions");

  await sendMockWSMessage(page, {
    type: "response",
    id: cmd.id,
    payload: { deletedIDs: ["race-ctrl-a", "race-ctrl-b"], failedIDs: [] },
  });

  await expect(page.getByTestId("session-race-ctrl-a")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("session-race-ctrl-b")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByTestId("session-race-ctrl-k")).toBeVisible();
  // No mid-flight switch here: the keep session stays active.
  expect(await currentHash(page)).toBe("#/race-ctrl-k");
});
