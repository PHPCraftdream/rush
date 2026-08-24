/**
 * Regression coverage for task #685 (F-1 of
 * docs/reviews/2026-08-24-twenty-eighth-review-3c6c56d4.md).
 *
 * useWS.ts's `messages_list` handler used to apply ANY incoming reply
 * unconditionally by session-ID match, with no msgID/sequence check. For an
 * externally-owned session (OwnedExternal === true, i.e. another `crush run`
 * process holds the session), TWO independent timers each fire their own
 * un-coordinated `load_messages` request for the SAME session ID:
 *
 *   - the dedicated "follow" poll, every 1.5s (pollMessagesIfFollowed,
 *     useWS.ts's FOLLOW_MESSAGES_POLL_MS timer)
 *   - a refresh piggy-backed on every sessions_list poll, every 5s
 *     (useWS.ts's OwnedExternal branch inside the "sessions_list" handler),
 *     explicitly there so the client never sits longer than the sessions
 *     poll interval without a fresh history read, in case the 1.5s poll
 *     missed a window
 *
 * Both requests go through the server's handleLoadMessages, dispatched onto
 * a 12-worker pool (hub.go's workerPoolSize) with NO ordering guarantee
 * between concurrently-queued items on different workers — two in-flight
 * load_messages requests for the same session can complete (and therefore
 * reply) in EITHER order, regardless of which was sent first.
 *
 * Fix (ws.ts's sendLoadMessages/isStaleMessagesReply): every load_messages
 * send is now tagged with a client-generated msgID, tracked as the latest
 * outstanding request per session. The messages_list handler drops any
 * reply whose id doesn't match the latest tracked request for its session,
 * so a stale (older) reply arriving after a newer one is discarded instead
 * of regressing the rendered transcript.
 *
 * This spec captures the REAL msgIDs the app generates for two genuinely
 * independent load_messages sends (one from each poller, exactly as they
 * fire in production), then delivers their replies out of send order —
 * proving both that the reproduction is faithful to the real send paths
 * and that the fix's id-matching logic (not a test-only stand-in) rejects
 * the stale one.
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend, clearWSSent } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
});

test("CONTROL: in-order load_messages replies render normally", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "reorder-ctrl", Title: "Ctrl", OwnedExternal: true })],
  });
  await expect(page.getByTestId("session-title-reorder-ctrl")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("session-reorder-ctrl").click();

  await clearWSSent(page);
  // Let the dedicated 1.5s follow-poll fire and capture its real msgID.
  const cmd = await waitForWSSend(page, "load_messages", 4000);
  expect(cmd.id, "load_messages must now carry a msgID (task #685 fix)").toBeTruthy();

  await sendMockWSMessage(page, {
    type: "messages_list",
    id: cmd.id,
    payload: {
      SessionID: "reorder-ctrl",
      Messages: [makeMessage({ ID: "m1", SessionID: "reorder-ctrl", Parts: [{ type: "text", Text: "First message" }] })],
    },
  });

  await expect(page.getByText("First message")).toBeVisible({ timeout: 2000 });
});

test("an older load_messages reply arriving after a newer one does not regress the visible chat", async ({ page }) => {
  // Freeze the page's timers so the real 1.5s/5s polling intervals
  // (useWS.ts's messagesInterval/listInterval, both wired via
  // window.setInterval) never fire during this test. Without this, they
  // keep running on real wall-clock time in the background and can slip
  // in a THIRD, uncontrolled load_messages send between the two captures
  // below (or between capture and reply), superseding both "OLD" and
  // "NEW" and making the test assert on the wrong pair of requests. The
  // two load_messages sends under test are triggered deterministically
  // (a click, a mock WS push) — nothing in this test depends on real time
  // elapsing, so freezing it removes the interference without weakening
  // the reproduction.
  await page.clock.install();
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "reorder-bug", Title: "Bug", OwnedExternal: true })],
  });
  await expect(page.getByTestId("session-title-reorder-bug")).toBeVisible({ timeout: 3000 });

  // Trigger two of useWS.ts's real, independent load_messages call sites
  // back-to-back, deterministically (not by waiting on the 1.5s/5s real
  // timers, which would keep firing concurrently and pollute the capture
  // with a THIRD, uncontrolled request racing in). Both sends below are
  // synchronous reactions to distinct events, exactly mirroring the two
  // uncoordinated pollers' shape (neither is aware of the other):
  //
  //   - clicking the session row -> Sidebar.selectSession -> sendLoadMessages
  //     (mirrors the dedicated follow-poll's send) -- request "OLD"
  //   - a fresh sessions_list push for an OwnedExternal active session ->
  //     the piggy-back branch inside useWS.ts's "sessions_list" handler ->
  //     sendLoadMessages, fired synchronously in the same handler, no timer
  //     involved -- request "NEW"
  await clearWSSent(page);
  await page.getByTestId("session-reorder-bug").click();
  const reqOld = await waitForWSSend(page, "load_messages", 2000);
  expect(reqOld.id, "load_messages must now carry a msgID (task #685 fix)").toBeTruthy();

  await clearWSSent(page);
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "reorder-bug", Title: "Bug", OwnedExternal: true })],
  });
  const reqNew = await waitForWSSend(page, "load_messages", 2000);
  expect(reqNew.id, "piggy-back load_messages must carry a msgID").toBeTruthy();
  expect(reqNew.id).not.toBe(reqOld.id);

  // Reply to the NEWER request FIRST: 3 messages, reflecting the latest
  // DB state (the external process kept writing). This is what a faster
  // worker-pool goroutine landing first looks like on the wire.
  await sendMockWSMessage(page, {
    type: "messages_list",
    id: reqNew.id,
    payload: {
      SessionID: "reorder-bug",
      Messages: [
        makeMessage({ ID: "m1", SessionID: "reorder-bug", Parts: [{ type: "text", Text: "First message" }] }),
        makeMessage({ ID: "m2", SessionID: "reorder-bug", Parts: [{ type: "text", Text: "Second message" }] }),
        makeMessage({ ID: "m3", SessionID: "reorder-bug", Parts: [{ type: "text", Text: "Third message (latest)" }] }),
      ],
    },
  });

  await expect(page.getByText("Third message (latest)")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Second message")).toBeVisible();

  // Now the OLDER request's reply lands SECOND — it was sent first, but
  // its worker-pool goroutine happened to finish later (hub.go's
  // workerPoolSize=12 parallel workers draining one shared queue with no
  // FIFO guarantee across workers). It carries a snapshot taken BEFORE
  // "Third message" existed.
  await sendMockWSMessage(page, {
    type: "messages_list",
    id: reqOld.id,
    payload: {
      SessionID: "reorder-bug",
      Messages: [
        makeMessage({ ID: "m1", SessionID: "reorder-bug", Parts: [{ type: "text", Text: "First message" }] }),
        makeMessage({ ID: "m2", SessionID: "reorder-bug", Parts: [{ type: "text", Text: "Second message" }] }),
      ],
    },
  });

  // Fixed behavior: the stale reply (id === reqOld.id, which is no longer
  // the latest tracked request for this session) is dropped by
  // isStaleMessagesReply, so "Third message (latest)" must still be
  // visible — the chat does not regress.
  await page.waitForTimeout(300);
  await expect(page.getByText("Third message (latest)")).toBeVisible();
  await expect(page.getByText("Second message")).toBeVisible();
  await expect(page.getByText("First message")).toBeVisible();
});
