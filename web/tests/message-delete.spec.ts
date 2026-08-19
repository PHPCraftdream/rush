/**
 * Single message deletion tests.
 *
 * Covers:
 *  - Delete button visible on hover
 *  - Clicking delete shows confirmation dialog
 *  - Confirm dialog Cancel dismisses without deleting
 *  - Confirm dialog Escape dismisses without deleting
 *  - Confirm dialog Delete sends delete_message WS command
 *  - message_deleted event removes message from UI
 *  - Delete button AND selection checkbox hidden for a still-streaming
 *    assistant message (task #595 — P1-1 of the 2026-08-19 static
 *    follow-up review: commit 547b0815 gated Edit on terminal Finish but
 *    left Delete, and the selection path into bulk delete, unguarded)
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

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
    payload: [makeSession({ ID: "del-sess", Title: "Delete Session" })],
  });
  await expect(page.getByText("Delete Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Delete Session").first().click();
  // useWS.ts's messages_list handler drops the payload unless each
  // message's SessionID matches the active session (stale in-flight
  // load_messages guard). makeMessage() defaults SessionID to "sess-1",
  // which never matches "del-sess", so it must be pinned here or every
  // assertion below times out waiting for text that was silently dropped.
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: messages.map((m) => ({ ...m, SessionID: "del-sess" })),
  });
}

// ── Delete button ────────────────────────────────────────────────────────

test("delete button appears on hover for user message", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "d-u1", Role: "user", Parts: [{ type: "text", Text: "Delete me user" }] }),
  ]);
  await expect(page.getByText("Delete me user")).toBeVisible({ timeout: 2000 });

  const msgRow = page.getByText("Delete me user").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await expect(msgRow.getByTitle("Delete")).toBeVisible({ timeout: 2000 });
});

// ── Confirm dialog ──────────────────────────────────────────────────────

test("clicking delete shows confirmation dialog", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "d-c1", Role: "user", Parts: [{ type: "text", Text: "Confirm delete" }] }),
  ]);

  const msgRow = page.getByText("Confirm delete").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Delete").click();

  await expect(page.getByText("Delete this message?")).toBeVisible({ timeout: 2000 });
  await expect(page.getByRole("button", { name: "Delete", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Cancel", exact: true })).toBeVisible();
});

test("clicking Cancel in confirm dialog dismisses without deleting", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "d-cc1", Role: "user", Parts: [{ type: "text", Text: "Cancel delete" }] }),
  ]);

  const msgRow = page.getByText("Cancel delete").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Delete").click();

  await expect(page.getByText("Delete this message?")).toBeVisible({ timeout: 2000 });
  await page.getByRole("button", { name: "Cancel" }).click();

  await expect(page.getByText("Delete this message?")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Cancel delete")).toBeVisible();

  const sent = await page.evaluate(() => {
    const s = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    return s.some((m) => m.type === "delete_message");
  });
  expect(sent).toBe(false);
});

test("pressing Escape in confirm dialog dismisses without deleting", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "d-ce1", Role: "user", Parts: [{ type: "text", Text: "Escape delete" }] }),
  ]);

  const msgRow = page.getByText("Escape delete").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Delete").click();

  await expect(page.getByText("Delete this message?")).toBeVisible({ timeout: 2000 });
  await page.keyboard.press("Escape");

  await expect(page.getByText("Delete this message?")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Escape delete")).toBeVisible();
});

test("clicking Delete in confirm dialog sends delete_message", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "d-del1", Role: "user", Parts: [{ type: "text", Text: "Really delete" }] }),
  ]);

  const msgRow = page.getByText("Really delete").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Delete").click();

  await expect(page.getByText("Delete this message?")).toBeVisible({ timeout: 2000 });
  await page.getByRole("button", { name: "Delete", exact: true }).click();

  const cmd = await waitForWSSend(page, "delete_message");
  expect((cmd.payload as { messageID: string }).messageID).toBe("d-del1");
});

// ── message_deleted event ───────────────────────────────────────────────

test("message_deleted event removes message from chat", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "d-ev1", Role: "user", Parts: [{ type: "text", Text: "Will be deleted" }] }),
    makeMessage({ ID: "d-ev2", Role: "assistant", Parts: [{ type: "text", Text: "Will remain" }] }),
  ]);

  await expect(page.getByText("Will be deleted")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Will remain")).toBeVisible();

  await sendMockWSMessage(page, {
    // useWS.ts's message_deleted handler also gates on SessionID matching
    // the active session (same guard shape as messages_list); an unscoped
    // payload is silently dropped rather than removing the message.
    type: "message_deleted",
    payload: { ID: "d-ev1", SessionID: "del-sess" },
  });

  await expect(page.getByText("Will be deleted")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Will remain")).toBeVisible();
});

// ── Streaming assistant message: Delete + selection gate (task #595) ────

test("delete button hidden for assistant message with no finish part (mid-stream, BUSY session)", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({
      ID: "d-stream1",
      Role: "assistant",
      Parts: [{ type: "text", Text: "Still streaming, no finish yet" }],
    }),
  ]);
  await expect(page.getByText("Still streaming, no finish yet")).toBeVisible({ timeout: 2000 });

  // Mark session as busy (simulating a live streaming turn)
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "del-sess", Busy: true },
  });

  const msgRow = page.getByText("Still streaming, no finish yet").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await expect(msgRow.getByTitle("Delete")).not.toBeVisible({ timeout: 2000 });
});

test("delete button hidden for assistant message with only a Partial finish (checkpoint ticker, BUSY session)", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({
      ID: "d-stream2",
      Role: "assistant",
      Parts: [
        { type: "text", Text: "Checkpointed mid-stream text" },
        { type: "finish", Reason: "", Message: "", Details: "", Partial: true },
      ],
    }),
  ]);
  await expect(page.getByText("Checkpointed mid-stream text")).toBeVisible({ timeout: 2000 });

  // Mark session as busy (simulating a live streaming turn)
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "del-sess", Busy: true },
  });

  const msgRow = page.getByText("Checkpointed mid-stream text").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await expect(msgRow.getByTitle("Delete")).not.toBeVisible({ timeout: 2000 });
});

test("delete button visible for a terminally finished assistant message", async ({ page }) => {
  // Control: once a real (non-Partial) Finish part lands, Delete must come
  // back — this is the same message shape message-edit.spec.ts uses to
  // prove Edit's gate is not permanently stuck closed.
  await setupWithMessages(page, [
    makeMessage({
      ID: "d-finished1",
      Role: "assistant",
      Parts: [
        { type: "text", Text: "Finished assistant message" },
        { type: "finish", Reason: "end_turn", Message: "", Details: "" },
      ],
    }),
  ]);
  await expect(page.getByText("Finished assistant message")).toBeVisible({ timeout: 2000 });

  const msgRow = page.getByText("Finished assistant message").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await expect(msgRow.getByTitle("Delete")).toBeVisible({ timeout: 2000 });
});

test("selection checkbox does not reveal on hover for a still-streaming assistant message (BUSY session)", async ({ page }) => {
  // The OTHER path into delete_messages besides the Trash button: if the
  // checkbox could still be checked, the message could still end up
  // selected and passed to the bulk delete_messages payload even with
  // Trash itself hidden. Message.tsx's checkboxVisible now folds in the
  // same streamGuardOK flag Trash and Edit use, so the wrap never gains
  // opacity-100 for a still-streaming assistant message in a BUSY session,
  // regardless of hover.
  await setupWithMessages(page, [
    makeMessage({
      ID: "d-stream3",
      Role: "assistant",
      Parts: [{ type: "text", Text: "Streaming, not selectable" }],
    }),
  ]);
  await expect(page.getByText("Streaming, not selectable")).toBeVisible({ timeout: 2000 });

  // Mark session as busy (simulating a live streaming turn)
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "del-sess", Busy: true },
  });

  const msgRow = page.getByText("Streaming, not selectable").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  const checkboxWrap = msgRow.locator(".msg-checkbox-wrap");
  await expect(checkboxWrap).toHaveClass(/opacity-0/);
  await msgRow.hover();
  // Unlike the finished-message case (see batch-selection.spec.ts's
  // "checkbox appears on message hover"), hovering a still-streaming
  // assistant message in a BUSY session must NOT reveal the checkbox.
  await expect(checkboxWrap).toHaveClass(/opacity-0/);
});

// ── Orphan assistant message (idle session, no finish part) ───────────────

test("delete button visible for an orphaned streaming assistant message (IDLE session)", async ({ page }) => {
  // Orphan: an assistant message that is NOT terminally finished AND its
  // session is NOT busy. This represents a message from a crashed/killed turn.
  // The server's delete handler force-deletes it after proving the session
  // is idle via IsSessionBusy.
  await setupWithMessages(page, [
    makeMessage({
      ID: "d-orphan1",
      Role: "assistant",
      Parts: [{ type: "text", Text: "Orphaned message, no finish part" }],
    }),
  ]);
  await expect(page.getByText("Orphaned message, no finish part")).toBeVisible({ timeout: 2000 });

  // NO agent_busy message sent — session is IDLE, so this is an orphan
  const msgRow = page.getByText("Orphaned message, no finish part").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();

  // Delete should be visible (orphan can be deleted)
  await expect(msgRow.getByTitle("Delete")).toBeVisible({ timeout: 2000 });

  // Edit should NOT be visible (server still refuses edits to any non-terminally-finished assistant message)
  await expect(msgRow.getByTitle("Edit")).not.toBeVisible({ timeout: 2000 });
});

test("clicking Delete on orphan sends delete_message", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({
      ID: "d-orphan-del",
      Role: "assistant",
      Parts: [{ type: "text", Text: "Orphan to delete" }],
    }),
  ]);
  await expect(page.getByText("Orphan to delete")).toBeVisible({ timeout: 2000 });

  const msgRow = page.getByText("Orphan to delete").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Delete").click();

  await expect(page.getByText("Delete this message?")).toBeVisible({ timeout: 2000 });
  await page.getByRole("button", { name: "Delete", exact: true }).click();

  const cmd = await waitForWSSend(page, "delete_message");
  expect((cmd.payload as { messageID: string }).messageID).toBe("d-orphan-del");
});

test("selection checkbox reveals on hover for an orphaned streaming assistant message (IDLE session)", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({
      ID: "d-orphan-sel",
      Role: "assistant",
      Parts: [{ type: "text", Text: "Orphaned message, selectable" }],
    }),
  ]);
  await expect(page.getByText("Orphaned message, selectable")).toBeVisible({ timeout: 2000 });

  // NO agent_busy message sent — session is IDLE, so this is an orphan
  const msgRow = page.getByText("Orphaned message, selectable").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  const checkboxWrap = msgRow.locator(".msg-checkbox-wrap");

  // Initially hidden
  await expect(checkboxWrap).toHaveClass(/opacity-0/);
  
  // Hover reveals the checkbox for orphan (selectable = true)
  await msgRow.hover();
  await expect(checkboxWrap).toHaveClass(/opacity-100/);
});

test("delete button hidden for orphan when session becomes BUSY", async ({ page }) => {
  // Control: busy state beats orphan — a message that would otherwise be
  // an orphan becomes non-deletable once the session is marked busy.
  await setupWithMessages(page, [
    makeMessage({
      ID: "d-orphan-busy",
      Role: "assistant",
      Parts: [{ type: "text", Text: "Orphan that becomes busy" }],
    }),
  ]);
  await expect(page.getByText("Orphan that becomes busy")).toBeVisible({ timeout: 2000 });

  const msgRow = page.getByText("Orphan that becomes busy").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  
  // Initially visible (orphan in idle session)
  await msgRow.hover();
  await expect(msgRow.getByTitle("Delete")).toBeVisible({ timeout: 2000 });

  // Now mark session as busy — delete should disappear
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "del-sess", Busy: true },
  });

  await expect(msgRow.getByTitle("Delete")).not.toBeVisible({ timeout: 2000 });
});
