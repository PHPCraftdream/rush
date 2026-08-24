/**
 * Regression coverage for task #689 (F1 of an external readonly review).
 *
 * This spec covers the live-push vs load_messages-reply race: the isStaleMessagesReply
 * guard from task #685 only orders load_messages REQUESTS; it cannot see
 * message_created/message_updated/message_deleted pushes applied between a request's
 * send and its reply. A still-latest reply carrying an older DB snapshot wholesale-
 * replaces $messages (or the sub-agent transcript via setSubAgentMessages) and erases
 * the fresher live state.
 *
 * Concrete failure pattern:
 *   1. Client sends load_messages (still-latest request per isStaleMessagesReply)
 *   2. Server reads DB snapshot with N messages
 *   3. A message_created broadcast for message N+1 arrives and is applied via upsertMessage
 *   4. The load_messages reply arrives (still passes staleness guard — no newer request)
 *      carrying only the N-message snapshot
 *   5. setMessages/setSubAgentMessages overwrites the list, erasing message N+1
 *
 * The same pattern applies to message_updated (stale content overwrites fresher streamed
 * content) and message_deleted (stale snapshot resurrects a deleted message).
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend, clearWSSent } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
});

test("a message_created push landing between load_messages send and its reply is not erased by the reply", async ({ page }) => {
  // Freeze the page's timers so the real 1.5s/5s polling intervals
  // (useWS.ts's messagesInterval/listInterval) never fire during this test.
  // Without this, they keep running on real wall-clock time and can slip in a
  // THIRD, uncontrolled load_messages send between the capture and the reply,
  // superseding our captured request and masking the race we're trying to
  // reproduce. The two events under test are deterministic (a click, a mock
  // WS push) — nothing in this test depends on real time elapsing, so freezing
  // it removes the interference without weakening the reproduction.
  await page.clock.install();
  await page.goto("/");

  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "live-race-1", Title: "LiveRace1" })],
  });
  await expect(page.getByTestId("session-title-live-race-1")).toBeVisible({ timeout: 3000 });

  await clearWSSent(page);
  await page.getByTestId("session-live-race-1").click();

  const req = await waitForWSSend(page, "load_messages", 2000);
  expect(req.id).toBeTruthy();

  // Inject a live push that arrives AFTER the load_messages request
  // but BEFORE its reply. This represents a message_created broadcast
  // from the server that lands while the server is still processing
  // the load_messages DB read.
  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessage({
      ID: "live-n1",
      SessionID: "live-race-1",
      Role: "user",
      Parts: [{ type: "text", Text: "LIVE-PUSH-CREATED-SENTINEL" }],
    }),
  });

  await expect(page.getByText("LIVE-PUSH-CREATED-SENTINEL")).toBeVisible({ timeout: 2000 });

  // NOW the delayed load_messages reply arrives carrying a DB snapshot
  // that predates the live push. Without the fix, this wholesale-replaces
  // $messages and erases the live-created message.
  await sendMockWSMessage(page, {
    type: "messages_list",
    id: req.id,
    payload: {
      SessionID: "live-race-1",
      Messages: [
        makeMessage({
          ID: "snap-1",
          SessionID: "live-race-1",
          Role: "user",
          Parts: [{ type: "text", Text: "SNAPSHOT-OLD-MESSAGE-SENTINEL" }],
        }),
      ],
    },
  });

  // The fix must preserve BOTH messages: the live-created one AND the
  // snapshot one. The wait mirrors the reorder spec to give a regression
  // time to wipe the live message before we assert.
  await page.waitForTimeout(300);
  await expect(page.getByText("LIVE-PUSH-CREATED-SENTINEL")).toBeVisible();
  await expect(page.getByText("SNAPSHOT-OLD-MESSAGE-SENTINEL")).toBeVisible();
});

test("a fresher streamed message_updated survives a staler snapshot reply for the same message", async ({ page }) => {
  await page.clock.install();
  await page.goto("/");

  const sessionID = "live-race-2";
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "LiveRace2" })],
  });
  await expect(page.getByTestId(`session-title-${sessionID}`)).toBeVisible({ timeout: 3000 });

  await clearWSSent(page);
  await page.getByTestId(`session-${sessionID}`).click();

  const req1 = await waitForWSSend(page, "load_messages", 2000);
  expect(req1.id).toBeTruthy();

  // Initial snapshot with short streamed text
  await sendMockWSMessage(page, {
    type: "messages_list",
    id: req1.id,
    payload: {
      SessionID: sessionID,
      Messages: [
        makeMessage({
          ID: "ma",
          SessionID: sessionID,
          Role: "assistant",
          Parts: [{ type: "text", Text: "STREAM-SHORT" }],
        }),
      ],
    },
  });

  await expect(page.getByText("STREAM-SHORT")).toBeVisible({ timeout: 2000 });

  // Trigger a second load_messages via the OwnedExternal piggy-back trick
  // (same as reorder spec). This simulates the 5s sessions_list poll.
  await clearWSSent(page);
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "LiveRace2", OwnedExternal: true })],
  });

  const req2 = await waitForWSSend(page, "load_messages", 2000);
  expect(req2.id).toBeTruthy();

  // Inject a live update with longer, fresher streamed content
  await sendMockWSMessage(page, {
    type: "message_updated",
    payload: makeMessage({
      ID: "ma",
      SessionID: sessionID,
      Role: "assistant",
      Parts: [{ type: "text", Text: "STREAM-SHORT plus more streamed content that grew" }],
    }),
  });

  await expect(page.getByText("STREAM-SHORT plus more streamed content that grew")).toBeVisible({ timeout: 2000 });

  // Reply with the STALE shorter snapshot (read happened before the live update)
  await sendMockWSMessage(page, {
    type: "messages_list",
    id: req2.id,
    payload: {
      SessionID: sessionID,
      Messages: [
        makeMessage({
          ID: "ma",
          SessionID: sessionID,
          Role: "assistant",
          Parts: [{ type: "text", Text: "STREAM-SHORT" }],
        }),
      ],
    },
  });

  // The fix must preserve the LONGER streamed text, not revert to the stale snapshot
  await page.waitForTimeout(300);
  await expect(page.getByText("STREAM-SHORT plus more streamed content that grew")).toBeVisible();
});

test("a message_deleted push is not undone by a snapshot reply whose read predated the delete", async ({ page }) => {
  await page.clock.install();
  await page.goto("/");

  const sessionID = "live-race-3";
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "LiveRace3" })],
  });
  await expect(page.getByTestId(`session-title-${sessionID}`)).toBeVisible({ timeout: 3000 });

  await clearWSSent(page);
  await page.getByTestId(`session-${sessionID}`).click();

  const req1 = await waitForWSSend(page, "load_messages", 2000);
  expect(req1.id).toBeTruthy();

  // Initial snapshot with three messages
  await sendMockWSMessage(page, {
    type: "messages_list",
    id: req1.id,
    payload: {
      SessionID: sessionID,
      Messages: [
        makeMessage({ ID: "del-1", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "DEL-KEEP-ONE" }] }),
        makeMessage({ ID: "del-2", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "DEL-VICTIM" }] }),
        makeMessage({ ID: "del-3", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "DEL-KEEP-THREE" }] }),
      ],
    },
  });

  await expect(page.getByText("DEL-KEEP-ONE")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("DEL-VICTIM")).toBeVisible();
  await expect(page.getByText("DEL-KEEP-THREE")).toBeVisible();

  // Trigger a second load_messages via OwnedExternal piggy-back
  await clearWSSent(page);
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "LiveRace3", OwnedExternal: true })],
  });

  const req2 = await waitForWSSend(page, "load_messages", 2000);
  expect(req2.id).toBeTruthy();

  // Inject a message_deleted push
  await sendMockWSMessage(page, {
    type: "message_deleted",
    payload: makeMessage({ ID: "del-2", SessionID: sessionID }),
  });

  await expect(page.getByText("DEL-VICTIM")).toHaveCount(0, { timeout: 2000 });

  // Reply with a snapshot that STILL CONTAINS the deleted message
  // (the DB read happened before the delete)
  await sendMockWSMessage(page, {
    type: "messages_list",
    id: req2.id,
    payload: {
      SessionID: sessionID,
      Messages: [
        makeMessage({ ID: "del-1", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "DEL-KEEP-ONE" }] }),
        makeMessage({ ID: "del-2", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "DEL-VICTIM" }] }),
        makeMessage({ ID: "del-3", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "DEL-KEEP-THREE" }] }),
      ],
    },
  });

  // The fix must keep the message DELETED, not resurrect it from the stale snapshot
  await page.waitForTimeout(300);
  await expect(page.getByText("DEL-VICTIM")).toHaveCount(0);
  await expect(page.getByText("DEL-KEEP-ONE")).toBeVisible();
  await expect(page.getByText("DEL-KEEP-THREE")).toBeVisible();
});

test("CONTROL: an epoch-clean reply (no live events since the request) still wholesale-replaces the list", async ({ page }) => {
  await page.clock.install();
  await page.goto("/");

  const sessionID = "live-race-4";
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "LiveRace4" })],
  });
  await expect(page.getByTestId(`session-title-${sessionID}`)).toBeVisible({ timeout: 3000 });

  await clearWSSent(page);
  await page.getByTestId(`session-${sessionID}`).click();

  const req1 = await waitForWSSend(page, "load_messages", 2000);
  expect(req1.id).toBeTruthy();

  // Initial snapshot with three messages
  await sendMockWSMessage(page, {
    type: "messages_list",
    id: req1.id,
    payload: {
      SessionID: sessionID,
      Messages: [
        makeMessage({ ID: "ctrl-1", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "CTRL-KEEP-ONE" }] }),
        makeMessage({ ID: "ctrl-2", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "CTRL-KEEP-TWO" }] }),
        makeMessage({ ID: "ctrl-3", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "CTRL-SHOULD-VANISH" }] }),
      ],
    },
  });

  await expect(page.getByText("CTRL-KEEP-ONE")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("CTRL-KEEP-TWO")).toBeVisible();
  await expect(page.getByText("CTRL-SHOULD-VANISH")).toBeVisible();

  // Trigger a second load_messages via OwnedExternal piggy-back
  // (NO pushes injected in between — this is the control case)
  await clearWSSent(page);
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "LiveRace4", OwnedExternal: true })],
  });

  const req2 = await waitForWSSend(page, "load_messages", 2000);
  expect(req2.id).toBeTruthy();

  // Reply with only two messages (compaction deleted the third)
  await sendMockWSMessage(page, {
    type: "messages_list",
    id: req2.id,
    payload: {
      SessionID: sessionID,
      Messages: [
        makeMessage({ ID: "ctrl-1", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "CTRL-KEEP-ONE" }] }),
        makeMessage({ ID: "ctrl-2", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "CTRL-KEEP-TWO" }] }),
      ],
    },
  });

  // Wholesale replace still works when no live events raced it —
  // the third message should be pruned
  await page.waitForTimeout(300);
  await expect(page.getByText("CTRL-SHOULD-VANISH")).toHaveCount(0);
  await expect(page.getByText("CTRL-KEEP-ONE")).toBeVisible();
  await expect(page.getByText("CTRL-KEEP-TWO")).toBeVisible();
});

test("sub-agent transcript: a live message_created push is not erased by the sub-session's delayed messages_list reply", async ({ page }) => {
  await page.clock.install();
  await page.goto("/");

  const sessionID = "sub-parent";
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "SubParent" })],
  });
  await expect(page.getByTestId(`session-title-${sessionID}`)).toBeVisible({ timeout: 3000 });

  await page.getByTestId(`session-${sessionID}`).click();

  // Wait for the parent session's load_messages
  const parentReq = await waitForWSSend(page, "load_messages", 2000);
  expect(parentReq.id).toBeTruthy();
  // Clear the capture buffer BEFORE delivering the parent reply: the
  // sub-agent block's lazy-load effect fires its sendLoadMessages the
  // moment the reply renders the agent tool_call (SubAgentBlock mounts),
  // and its requested-latch prevents it from ever re-firing — clearing
  // after the block becomes visible would wipe the sub request from
  // __wsSent and leave nothing to capture.
  await clearWSSent(page);

  // Reply with a parent message containing an agent tool_call
  // IMPORTANT: do NOT add a terminal finish part — a terminal finish makes
  // SubAgentBlock treat the parent as done and collapse the body; we need it open.
  await sendMockWSMessage(page, {
    type: "messages_list",
    id: parentReq.id,
    payload: {
      SessionID: sessionID,
      Messages: [
        makeMessage({
          ID: "pm1",
          SessionID: sessionID,
          Role: "assistant",
          Parts: [
            {
              type: "tool_call",
              ID: "call_1",
              Name: "agent",
              Input: '{"prompt":"sub agent race work"}',
              Finished: true,
            },
          ],
        }),
      ],
    },
  });

  // Wait for the sub-agent block to appear
  await expect(page.locator(".sub-agent-block")).toBeVisible({ timeout: 3000 });

  // The block's lazy-load effect fired sendLoadMessages for subSessionID
  // "pm1$$call_1" on mount (buffer was cleared just before the parent
  // reply, so this capture sees exactly that request).
  const subReq = await waitForWSSend(page, "load_messages", 3000);
  expect(subReq.id).toBeTruthy();
  expect(subReq.payload).toEqual({ sessionID: "pm1$$call_1" });

  // Inject a live push for the sub-agent session
  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessage({
      ID: "sub-live-1",
      SessionID: "pm1$$call_1",
      Role: "assistant",
      Parts: [{ type: "text", Text: "SUB-LIVE-PUSH-SENTINEL" }],
    }),
  });

  await expect(page.getByText("SUB-LIVE-PUSH-SENTINEL")).toBeVisible({ timeout: 2000 });

  // Reply with a snapshot that predates the live push
  await sendMockWSMessage(page, {
    type: "messages_list",
    id: subReq.id,
    payload: {
      SessionID: "pm1$$call_1",
      Messages: [
        makeMessage({
          ID: "sub-old-1",
          SessionID: "pm1$$call_1",
          Role: "assistant",
          Parts: [{ type: "text", Text: "SUB-SNAPSHOT-SENTINEL" }],
        }),
      ],
    },
  });

  // The fix must preserve BOTH the live push AND the snapshot in the sub-agent transcript
  await page.waitForTimeout(300);
  await expect(page.getByText("SUB-LIVE-PUSH-SENTINEL")).toBeVisible();
  await expect(page.getByText("SUB-SNAPSHOT-SENTINEL")).toBeVisible();
});
