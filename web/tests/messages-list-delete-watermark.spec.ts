/**
 * Regression coverage for task #731 (delete-resurrection risk design doc),
 * updated for task #737's generation-counter fix.
 *
 * mergeMessageLists (web/src/store.ts) protects live-applied deletes via
 * per-session tombstones, cleared whenever a `messages_list` reply looks
 * "epoch-clean" (ws.ts's hasLiveEventsSinceRequest: no live push has been
 * applied since the request was SENT). That counter proves something about
 * wall-clock event ordering, but nothing about whether the reply's actual
 * DB read postdates a delete the client already knows about — the server
 * dispatches load_messages across a worker pool with no FIFO guarantee and
 * reads off a SEPARATE read-only connection pool (internal/db.ConnectRead),
 * so a reply can be "the latest request, no push raced it" and STILL carry
 * a DB snapshot read before an earlier delete's commit.
 *
 * Concrete failure pattern this spec reproduces:
 *   1. Client loads a session; snapshot contains message X.
 *   2. message_deleted for X arrives (with its DeleteGeneration watermark)
 *      — this bumps the live-event epoch to 1 and tombstones X.
 *   3. ONLY AFTER that push lands does the client send a fresh
 *      load_messages (e.g. the 5s sessions_list piggy-back poll). Its
 *      requestEpoch is captured as 1 — i.e. the SAME epoch the delete
 *      already bumped to. No further push arrives before the reply.
 *   4. The reply lands. hasLiveEventsSinceRequest is FALSE (epoch didn't
 *      move again after send) — by the OLD heuristic alone this reply is
 *      "clean", so applyMessagesSnapshot would wholesale-replace $messages
 *      AND clear the tombstone for X. If the reply's snapshot still
 *      contains X (its own DB read predated the delete — the read-pool/
 *      worker-pool skew described above), X resurrects.
 *
 * The watermark fix closes this: the reply now also carries a `Watermark`
 * (the session's delete-generation counter as of that read — task #737;
 * originally the session's max message rowid, replaced because that did
 * not advance on a non-tail delete). isSnapshotStaleForDeletes compares it
 * against the delete high-water mark recorded from step 2's push. Here the
 * reply's Watermark is deliberately set LOWER than the delete's
 * DeleteGeneration, proving the read predates the delete regardless of what
 * the epoch counter says — the merge path must be taken and X must stay
 * deleted.
 *
 * The CONTROL test at the bottom proves the fix does not regress the
 * ordinary case: a reply whose Watermark is fresh (>= the delete's
 * DeleteGeneration) still takes the wholesale-replace path exactly as
 * before.
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend, clearWSSent } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
});

test("an epoch-clean reply with a STALE watermark does not resurrect an already-deleted message", async ({ page }) => {
  await page.clock.install();
  await page.goto("/");

  const sessionID = "watermark-1";
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "Watermark1" })],
  });
  await expect(page.getByTestId(`session-title-${sessionID}`)).toBeVisible({ timeout: 3000 });

  await clearWSSent(page);
  await page.getByTestId(`session-${sessionID}`).click();

  const req1 = await waitForWSSend(page, "load_messages", 2000);
  expect(req1.id).toBeTruthy();

  // Initial snapshot: three messages, watermark 3 (delete-generation counter
  // for this session as of this read — three prior deletes already landed).
  await sendMockWSMessage(page, {
    type: "messages_list",
    id: req1.id,
    payload: {
      SessionID: sessionID,
      Watermark: 3,
      Messages: [
        makeMessage({ ID: "wm-1", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "WM-KEEP-ONE" }] }),
        makeMessage({ ID: "wm-2", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "WM-VICTIM" }] }),
        makeMessage({ ID: "wm-3", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "WM-KEEP-THREE" }] }),
      ],
    },
  });

  await expect(page.getByText("WM-KEEP-ONE")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("WM-VICTIM")).toBeVisible();
  await expect(page.getByText("WM-KEEP-THREE")).toBeVisible();

  // Delete wm-2, with its watermark: DeleteGeneration 5 (newer than the
  // initial snapshot's watermark of 3, as a real delete committed after
  // that read would be — two more deletes happened elsewhere in between).
  // This both tombstones wm-2 AND bumps the live-event epoch.
  await sendMockWSMessage(page, {
    type: "message_deleted",
    payload: makeMessage({ ID: "wm-2", SessionID: sessionID, DeleteGeneration: 5 }),
  });
  await expect(page.getByText("WM-VICTIM")).toHaveCount(0, { timeout: 2000 });

  // ONLY NOW does the client send a fresh load_messages (mirrors the 5s
  // sessions_list piggy-back poll firing sometime after the delete push
  // already landed) — its requestEpoch is captured AT the already-bumped
  // epoch, so no further live event needs to fire before the reply for
  // hasLiveEventsSinceRequest to report "clean".
  await clearWSSent(page);
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "Watermark1", OwnedExternal: true })],
  });
  const req2 = await waitForWSSend(page, "load_messages", 2000);
  expect(req2.id).toBeTruthy();

  // Reply arrives with NO further live push in between (epoch-clean by the
  // OLD heuristic) but its Watermark (4) is STALE relative to the delete's
  // DeleteGeneration (5) — this specific read genuinely predated the delete
  // commit (worker-pool / separate-read-pool skew), and its snapshot still
  // contains wm-2. Without the watermark fix, applyMessagesSnapshot would
  // take the "clean" branch: wholesale-replace + clear the tombstone,
  // resurrecting WM-VICTIM.
  await sendMockWSMessage(page, {
    type: "messages_list",
    id: req2.id,
    payload: {
      SessionID: sessionID,
      Watermark: 4,
      Messages: [
        makeMessage({ ID: "wm-1", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "WM-KEEP-ONE" }] }),
        makeMessage({ ID: "wm-2", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "WM-VICTIM" }] }),
        makeMessage({ ID: "wm-3", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "WM-KEEP-THREE" }] }),
      ],
    },
  });

  // The fix must keep wm-2 deleted despite the epoch-clean verdict, because
  // the watermark proves the read is stale relative to the recorded delete.
  await page.waitForTimeout(300);
  await expect(page.getByText("WM-VICTIM")).toHaveCount(0);
  await expect(page.getByText("WM-KEEP-ONE")).toBeVisible();
  await expect(page.getByText("WM-KEEP-THREE")).toBeVisible();
});

test("CONTROL: an epoch-clean reply with a FRESH watermark still wholesale-replaces (compaction converges normally)", async ({ page }) => {
  await page.clock.install();
  await page.goto("/");

  const sessionID = "watermark-2";
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "Watermark2" })],
  });
  await expect(page.getByTestId(`session-title-${sessionID}`)).toBeVisible({ timeout: 3000 });

  await clearWSSent(page);
  await page.getByTestId(`session-${sessionID}`).click();

  const req1 = await waitForWSSend(page, "load_messages", 2000);
  expect(req1.id).toBeTruthy();

  await sendMockWSMessage(page, {
    type: "messages_list",
    id: req1.id,
    payload: {
      SessionID: sessionID,
      Watermark: 1,
      Messages: [
        makeMessage({ ID: "ctrl-1", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "WMC-KEEP-ONE" }] }),
        makeMessage({ ID: "ctrl-2", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "WMC-SHOULD-VANISH" }] }),
      ],
    },
  });
  await expect(page.getByText("WMC-KEEP-ONE")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("WMC-SHOULD-VANISH")).toBeVisible();

  // Delete ctrl-2, watermark DeleteGeneration 2.
  await sendMockWSMessage(page, {
    type: "message_deleted",
    payload: makeMessage({ ID: "ctrl-2", SessionID: sessionID, DeleteGeneration: 2 }),
  });
  await expect(page.getByText("WMC-SHOULD-VANISH")).toHaveCount(0, { timeout: 2000 });

  await clearWSSent(page);
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "Watermark2", OwnedExternal: true })],
  });
  const req2 = await waitForWSSend(page, "load_messages", 2000);
  expect(req2.id).toBeTruthy();

  // Reply's Watermark (3) is FRESH — >= the delete's DeleteGeneration (2) —
  // meaning this read genuinely postdates the delete, matching its
  // snapshot correctly excluding ctrl-2. Epoch-clean AND watermark-fresh:
  // the wholesale-replace path must still run normally.
  await sendMockWSMessage(page, {
    type: "messages_list",
    id: req2.id,
    payload: {
      SessionID: sessionID,
      Watermark: 3,
      Messages: [
        makeMessage({ ID: "ctrl-1", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "WMC-KEEP-ONE" }] }),
      ],
    },
  });

  await page.waitForTimeout(300);
  await expect(page.getByText("WMC-SHOULD-VANISH")).toHaveCount(0);
  await expect(page.getByText("WMC-KEEP-ONE")).toBeVisible();
});
