/**
 * Regression test for mergePreserveContent guard in web/src/store.ts.
 *
 * Guards against a bug (Round 24, F-1, P1) where mergePreserveContent blocked
 * ANY assistant message_updated whose total thinking or text shrank, so a
 * successful part delete/edit never reached the DOM: deleted thinking
 * resurrected, shorter edits silently reverted, and the client's Parts array
 * diverged from the DB's so a repeat delete click sent a partIndex that hit a
 * DIFFERENT part server-side.
 *
 * The fix: the guard now applies ONLY when the incoming message is NOT
 * terminally finished (isTerminallyFinished — a finish part with Partial falsy
 * means terminal). Terminal shrinks (i.e., after a successful edit/delete
 * operation) are taken verbatim.
 *
 * This spec pins that fix AND the invariant that mid-stream (Partial) stale
 * snapshots are still guarded.
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend, clearWSSent } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

const TERMINAL_FINISH = { type: "finish", Reason: "end_turn", Message: "", Details: "" };

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

async function setupWithMessages(
  page: import("@playwright/test").Page,
  messages: ReturnType<typeof makeMessage>[]
) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "mg-sess", Title: "MergeGuard Session" })],
  });
  await expect(page.getByText("MergeGuard Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("MergeGuard Session").first().click();
  // Pin SessionID to "mg-sess" for all messages (see comment in thinking-parts.spec.ts)
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: messages.map((m) => ({ ...m, SessionID: "mg-sess" })),
  });
}

// ── Test (a): Deleting a thinking row removes it from DOM and does not resurrect it

test("deleting a thinking row removes it from the DOM and does not resurrect it", async ({ page }) => {
  const MESSAGE_ID = "mg-msg-a";
  const THINKING_SENTINEL = "MG-REASONING-SENTINEL-ABC";
  const NARRATION_SENTINEL = "MG-NARRATION-SENTINEL-ABC";

  await setupWithMessages(page, [
    makeMessage({ ID: "u1", Role: "user", Parts: [{ type: "text", Text: "go" }] }),
    makeMessage({
      ID: MESSAGE_ID,
      Role: "assistant",
      Parts: [
        { type: "thinking", Thinking: THINKING_SENTINEL },
        { type: "text", Text: NARRATION_SENTINEL },
        { type: "tool_call", ID: "tc-a", Name: "bash", Input: '{"command":"ls"}', Finished: true },
        TERMINAL_FINISH,
      ],
    }),
  ]);

  await clearWSSent(page);

  // Find the thinking row and delete it
  const row = page.locator('[data-test-id="action-row"]').filter({ hasText: THINKING_SENTINEL });
  await row.hover();
  await row.getByTitle("Delete thinking").click();
  await page.locator(".modal-panel button", { hasText: "Delete" }).click();

  // The delete_message_part command must have payload with messageID and partIndex: 0
  const cmd = await waitForWSSend(page, "delete_message_part");
  expect(cmd.payload).toEqual({ messageID: MESSAGE_ID, partIndex: 0 });

  // Inject the message_updated broadcast WITHOUT the thinking part (server applied the delete)
  await sendMockWSMessage(page, {
    type: "message_updated",
    payload: {
      ...makeMessage({
        ID: MESSAGE_ID,
        Role: "assistant",
        SessionID: "mg-sess",
        Parts: [
          { type: "text", Text: NARRATION_SENTINEL },
          { type: "tool_call", ID: "tc-a", Name: "bash", Input: '{"command":"ls"}', Finished: true },
          TERMINAL_FINISH,
        ],
      }),
    },
  });

  // Assert: no action-row containing the thinking sentinel remains
  await expect(page.locator('[data-test-id="action-row"]').filter({ hasText: THINKING_SENTINEL })).toHaveCount(0);

  // Assert: the narration text sentinel is still visible
  await expect(page.getByText(NARRATION_SENTINEL)).toBeVisible();

  // Re-assert after a short wait to confirm it does not resurrect
  await page.waitForTimeout(250);
  await expect(page.locator('[data-test-id="action-row"]').filter({ hasText: THINKING_SENTINEL })).toHaveCount(0);
  await expect(page.getByText(NARRATION_SENTINEL)).toBeVisible();
});

// ── Test (b): A second click after a successful delete cannot silently corrupt an unrelated part

test("a second click after a successful delete cannot silently corrupt an unrelated part", async ({ page }) => {
  const MESSAGE_ID = "mg-msg-b";
  const THINKING_SENTINEL = "MG-REASONING-SENTINEL-DEF";
  const NARRATION_SENTINEL = "MG-NARRATION-SENTINEL-DEF";

  await setupWithMessages(page, [
    makeMessage({ ID: "u1", Role: "user", Parts: [{ type: "text", Text: "go" }] }),
    makeMessage({
      ID: MESSAGE_ID,
      Role: "assistant",
      Parts: [
        { type: "thinking", Thinking: THINKING_SENTINEL },
        { type: "text", Text: NARRATION_SENTINEL },
        { type: "tool_call", ID: "tc-b", Name: "bash", Input: '{"command":"ls"}', Finished: true },
        TERMINAL_FINISH,
      ],
    }),
  ]);

  await clearWSSent(page);

  // Delete the thinking row
  const row = page.locator('[data-test-id="action-row"]').filter({ hasText: THINKING_SENTINEL });
  await row.hover();
  await row.getByTitle("Delete thinking").click();
  await page.locator(".modal-panel button", { hasText: "Delete" }).click();

  const cmd = await waitForWSSend(page, "delete_message_part");
  expect(cmd.payload).toEqual({ messageID: MESSAGE_ID, partIndex: 0 });

  // Inject the message_updated broadcast WITHOUT the thinking part
  await sendMockWSMessage(page, {
    type: "message_updated",
    payload: {
      ...makeMessage({
        ID: MESSAGE_ID,
        Role: "assistant",
        SessionID: "mg-sess",
        Parts: [
          { type: "text", Text: NARRATION_SENTINEL },
          { type: "tool_call", ID: "tc-b", Name: "bash", Input: '{"command":"ls"}', Finished: true },
          TERMINAL_FINISH,
        ],
      }),
    },
  });

  // Assert: the thinking row locator has count 0 (nothing left to click)
  await expect(page.locator('[data-test-id="action-row"]').filter({ hasText: THINKING_SENTINEL })).toHaveCount(0);

  // Assert: the narration text is STILL visible (unrelated part was not corrupted)
  await expect(page.getByText(NARRATION_SENTINEL)).toBeVisible();

  // Assert: window.__wsSent contains EXACTLY ONE delete_message_part
  const sentCount = await page.evaluate(() => {
    const sent = (window as any).__wsSent || [];
    return sent.filter((m: any) => m.type === "delete_message_part").length;
  });
  expect(sentCount).toBe(1);
});

// ── Test (c): Editing text to something SHORTER updates the DOM

test("editing thinking to something SHORTER updates the DOM", async ({ page }) => {
  const MESSAGE_ID = "mg-msg-c";
  const LONG_THINKING = "MG-REASONING-LONG-SENTINEL-GHI-This-is-a-very-long-piece-of-reasoning-that-will-be-shortened";
  const SHORT_THINKING = "MG-SHORT-GHI";

  await setupWithMessages(page, [
    makeMessage({ ID: "u1", Role: "user", Parts: [{ type: "text", Text: "go" }] }),
    makeMessage({
      ID: MESSAGE_ID,
      Role: "assistant",
      Parts: [
        { type: "thinking", Thinking: LONG_THINKING },
        { type: "text", Text: "MG-NARRATION-SENTINEL-GHI" },
        { type: "tool_call", ID: "tc-c", Name: "bash", Input: '{"command":"ls"}', Finished: true },
        TERMINAL_FINISH,
      ],
    }),
  ]);

  await clearWSSent(page);

  // Expand the thinking row first (EditForm only renders when expanded)
  const row = page.locator('[data-test-id="action-row"]').filter({ hasText: LONG_THINKING });
  await row.locator('[data-test-id="action-row-toggle"]').click();
  await row.hover();
  await row.getByTitle("Edit thinking").click();
  await row.locator("textarea").fill(SHORT_THINKING);
  await row.locator("button", { hasText: "Save" }).click();

  // The update_message_part command must have the correct payload
  const cmd = await waitForWSSend(page, "update_message_part");
  expect(cmd.payload).toEqual({ messageID: MESSAGE_ID, partIndex: 0, content: SHORT_THINKING });

  // Inject the message_updated broadcast with the shortened thinking
  await sendMockWSMessage(page, {
    type: "message_updated",
    payload: {
      ...makeMessage({
        ID: MESSAGE_ID,
        Role: "assistant",
        SessionID: "mg-sess",
        Parts: [
          { type: "thinking", Thinking: SHORT_THINKING },
          { type: "text", Text: "MG-NARRATION-SENTINEL-GHI" },
          { type: "tool_call", ID: "tc-c", Name: "bash", Input: '{"command":"ls"}', Finished: true },
          TERMINAL_FINISH,
        ],
      }),
    },
  });

  // Assert: the new short text is visible in the action-row
  await expect(page.locator('[data-test-id="action-row"]').filter({ hasText: SHORT_THINKING })).toBeVisible();

  // Assert: the OLD long sentinel is gone from the DOM
  await expect(page.locator('[data-test-id="action-row"]').filter({ hasText: LONG_THINKING })).toHaveCount(0);
});

// ── Test (d): Control — editing to something LONGER still works

test("editing thinking to something LONGER still works", async ({ page }) => {
  const MESSAGE_ID = "mg-msg-d";
  const SHORT_THINKING = "MG-SHORT-JKL";
  const LONG_THINKING = "MG-LONG-SENTINEL-JKL-This-is-a-much-longer-piece-of-reasoning-that-replaces-the-short-one";

  await setupWithMessages(page, [
    makeMessage({ ID: "u1", Role: "user", Parts: [{ type: "text", Text: "go" }] }),
    makeMessage({
      ID: MESSAGE_ID,
      Role: "assistant",
      Parts: [
        { type: "thinking", Thinking: SHORT_THINKING },
        { type: "text", Text: "MG-NARRATION-SENTINEL-JKL" },
        { type: "tool_call", ID: "tc-d", Name: "bash", Input: '{"command":"ls"}', Finished: true },
        TERMINAL_FINISH,
      ],
    }),
  ]);

  await clearWSSent(page);

  // Expand the thinking row first
  const row = page.locator('[data-test-id="action-row"]').filter({ hasText: SHORT_THINKING });
  await row.locator('[data-test-id="action-row-toggle"]').click();
  await row.hover();
  await row.getByTitle("Edit thinking").click();
  await row.locator("textarea").fill(LONG_THINKING);
  await row.locator("button", { hasText: "Save" }).click();

  // The update_message_part command must have the correct payload
  const cmd = await waitForWSSend(page, "update_message_part");
  expect(cmd.payload).toEqual({ messageID: MESSAGE_ID, partIndex: 0, content: LONG_THINKING });

  // Inject the message_updated broadcast with the longer thinking
  await sendMockWSMessage(page, {
    type: "message_updated",
    payload: {
      ...makeMessage({
        ID: MESSAGE_ID,
        Role: "assistant",
        SessionID: "mg-sess",
        Parts: [
          { type: "thinking", Thinking: LONG_THINKING },
          { type: "text", Text: "MG-NARRATION-SENTINEL-JKL" },
          { type: "tool_call", ID: "tc-d", Name: "bash", Input: '{"command":"ls"}', Finished: true },
          TERMINAL_FINISH,
        ],
      }),
    },
  });

  // Assert: the longer text is visible in the action-row
  await expect(page.locator('[data-test-id="action-row"]').filter({ hasText: LONG_THINKING })).toBeVisible();

  // Assert: the short sentinel is gone from the DOM
  await expect(page.locator('[data-test-id="action-row"]').filter({ hasText: SHORT_THINKING })).toHaveCount(0);
});

// ── Test (e): Invariant — mid-stream stale snapshot is still guarded

test("invariant — mid-stream stale snapshot is still guarded", async ({ page }) => {
  const MESSAGE_ID = "mg-msg-e";
  const LONG_THINKING = "MG-REASONING-LONG-MNO-This-is-a-very-long-piece-of-reasoning";
  const SHORT_THINKING = "MG-SHORT-MNO";
  const PARTIAL_FINISH = { type: "finish", Reason: "end_turn", Message: "", Details: "", Partial: true };

  await setupWithMessages(page, [
    makeMessage({ ID: "u1", Role: "user", Parts: [{ type: "text", Text: "go" }] }),
    makeMessage({
      ID: MESSAGE_ID,
      Role: "assistant",
      Parts: [
        { type: "thinking", Thinking: LONG_THINKING },
        { type: "tool_call", ID: "tc-e", Name: "bash", Input: '{"command":"ls"}', Finished: true },
        PARTIAL_FINISH,
      ],
    }),
  ]);

  // Inject a message_updated with a SHORTER thinking (stale snapshot) and still Partial finish
  await sendMockWSMessage(page, {
    type: "message_updated",
    payload: {
      ...makeMessage({
        ID: MESSAGE_ID,
        Role: "assistant",
        SessionID: "mg-sess",
        Parts: [
          { type: "thinking", Thinking: SHORT_THINKING },
          { type: "tool_call", ID: "tc-e", Name: "bash", Input: '{"command":"ls"}', Finished: true },
          PARTIAL_FINISH,
        ],
      }),
    },
  });

  // Assert: the DOM still shows the LONG sentinel text in the action-row (stale snapshot was guarded)
  await expect(page.locator('[data-test-id="action-row"]').filter({ hasText: LONG_THINKING })).toBeVisible();

  // Assert: the shorter text is NOT visible in the DOM
  await expect(page.locator('[data-test-id="action-row"]').filter({ hasText: SHORT_THINKING })).toHaveCount(0);
});
