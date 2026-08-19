/**
 * Message editing tests.
 *
 * Covers:
 *  - Edit button visible on hover for user messages
 *  - Edit button visible on hover for assistant messages
 *  - Clicking edit opens textarea with message content
 *  - Escape cancels edit without sending
 *  - Ctrl+Enter commits edit and sends update_message_content
 *  - Save button commits edit
 *  - Cancel button cancels edit
 *  - Unchanged content does not send WS command
 *  - Empty content does not send WS command
 *  - Edit button hidden for a still-streaming assistant message (task #590)
 *  - Delete button ALSO hidden for a still-streaming assistant message
 *    (task #595 — the P1-1 gap the edit-only guard left open)
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

async function setupWithMessage(
  page: import("@playwright/test").Page,
  msg: ReturnType<typeof makeMessage>
) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "edit-sess", Title: "Edit Session" })],
  });
  await expect(page.getByText("Edit Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Edit Session").first().click();
  // useWS.ts's messages_list handler drops the payload unless its SessionID
  // matches the active session. makeMessage() defaults SessionID to
  // "sess-1", which never matches "edit-sess", so it must be pinned here.
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [{ ...msg, SessionID: "edit-sess" }],
  });
}

// ── Edit button visibility ──────────────────────────────────────────────────

test("edit button appears on hover for user message", async ({ page }) => {
  const msg = makeMessage({
    ID: "eu-1",
    Role: "user",
    Parts: [{ type: "text", Text: "Editable user message" }],
  });
  await setupWithMessage(page, msg);
  await expect(page.getByText("Editable user message")).toBeVisible({ timeout: 2000 });

  const msgRow = page.getByText("Editable user message").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await expect(msgRow.getByTitle("Edit")).toBeVisible({ timeout: 2000 });
});

test("edit button appears on hover for assistant message", async ({ page }) => {
  const msg = makeMessage({
    ID: "ea-1",
    Role: "assistant",
    // Assistant messages only show the Edit control once terminally
    // finished (task #590): a message still mid-stream would have its edit
    // silently overwritten by the turn's next checkpoint or terminal write,
    // so a real Finish part (Partial unset/false) is required here.
    Parts: [
      { type: "text", Text: "Editable assistant message" },
      { type: "finish", Reason: "end_turn", Message: "", Details: "" },
    ],
  });
  await setupWithMessage(page, msg);
  await expect(page.getByText("Editable assistant message")).toBeVisible({ timeout: 2000 });

  const msgRow = page.getByText("Editable assistant message").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await expect(msgRow.getByTitle("Edit")).toBeVisible({ timeout: 2000 });
});

// Task #590: the server refuses edits to an assistant message that is still
// mid-stream (no terminal Finish part, or only a Partial one from the
// auto-checkpoint ticker) because the agent turn's next checkpoint or
// terminal write would silently overwrite the edit. The Edit control must
// not even be offered for such a message.
//
// Task #595 (P1-1 of the 2026-08-19 static follow-up review): commit
// 547b0815 closed the edit path above but left delete unguarded — this test
// used to assert "Delete must remain available — only Edit is gated", which
// was exactly the gap the review found: the server now ALSO refuses to
// delete a still-streaming assistant message (message.Service.Delete /
// DeleteMessageIfTerminal), for the same reason (the live turn's terminal
// write races a delete and can "resurrect" the message in the UI). Delete
// must be hidden here too, not left available.
//
// With the orphan exception (task #595 follow-up): an unfinished assistant
// message in an IDLE session (i.e., the turn crashed or was killed) can be
// deleted, so we now mark the session as BUSY in this test to keep Delete
// hidden. Edit stays hidden regardless.
test("edit AND delete buttons hidden for assistant message with no finish part (mid-stream, BUSY session, pre-checkpoint)", async ({ page }) => {
  const msg = makeMessage({
    ID: "stream-1",
    Role: "assistant",
    Parts: [{ type: "text", Text: "Still streaming, no finish yet" }],
  });
  await setupWithMessage(page, msg);
  await expect(page.getByText("Still streaming, no finish yet")).toBeVisible({ timeout: 2000 });

  // Mark session as busy (simulating a live streaming turn)
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "edit-sess", Busy: true },
  });

  const msgRow = page.getByText("Still streaming, no finish yet").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await expect(msgRow.getByTitle("Edit")).not.toBeVisible({ timeout: 2000 });
  await expect(msgRow.getByTitle("Delete")).not.toBeVisible({ timeout: 2000 });
});

// Task #595: this is the more realistic mid-stream shape (a Partial Finish
// from the ~2s auto-checkpoint ticker, not the "no Finish part at all" case
// above) and the same gate must hide Delete here too — a checkpointed but
// not-yet-terminal message is exactly what the server-side
// DeleteMessageIfTerminal predicate refuses.
//
// With the orphan exception (task #595 follow-up): an unfinished assistant
// message in an IDLE session (i.e., the turn crashed or was killed) can be
// deleted, so we now mark the session as BUSY in this test to keep Delete
// hidden. Edit stays hidden regardless.
test("edit AND delete buttons hidden for assistant message with only a Partial finish (checkpoint ticker, BUSY session)", async ({ page }) => {
  const msg = makeMessage({
    ID: "stream-2",
    Role: "assistant",
    Parts: [
      { type: "text", Text: "Checkpointed mid-stream text" },
      { type: "finish", Reason: "", Message: "", Details: "", Partial: true },
    ],
  });
  await setupWithMessage(page, msg);
  await expect(page.getByText("Checkpointed mid-stream text")).toBeVisible({ timeout: 2000 });

  // Mark session as busy (simulating a live streaming turn)
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "edit-sess", Busy: true },
  });

  const msgRow = page.getByText("Checkpointed mid-stream text").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await expect(msgRow.getByTitle("Edit")).not.toBeVisible({ timeout: 2000 });
  await expect(msgRow.getByTitle("Delete")).not.toBeVisible({ timeout: 2000 });
});

// Orphan exception (task #595 follow-up): an unfinished assistant message
// in an IDLE session (i.e., the turn crashed or was killed) can be deleted,
// but Edit is still hidden because the server refuses edits to any
// non-terminally-finished assistant message.
test("edit hidden but delete visible for orphaned streaming assistant message (IDLE session)", async ({ page }) => {
  const msg = makeMessage({
    ID: "orphan-1",
    Role: "assistant",
    Parts: [{ type: "text", Text: "Orphaned message" }],
  });
  await setupWithMessage(page, msg);
  await expect(page.getByText("Orphaned message")).toBeVisible({ timeout: 2000 });

  // NO agent_busy message sent — session is IDLE, so this is an orphan
  const msgRow = page.getByText("Orphaned message").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  
  // Edit should still be hidden (server refuses edits to any non-terminally-finished assistant message)
  await expect(msgRow.getByTitle("Edit")).not.toBeVisible({ timeout: 2000 });
  
  // Delete should be visible (orphan can be deleted)
  await expect(msgRow.getByTitle("Delete")).toBeVisible({ timeout: 2000 });
});

// ── Starting edit ─────────────────────────────────────────────────────────

test("clicking edit opens textarea with current message content", async ({ page }) => {
  const msg = makeMessage({
    ID: "es-1",
    Role: "user",
    Parts: [{ type: "text", Text: "Original content" }],
  });
  await setupWithMessage(page, msg);
  await expect(page.getByText("Original content")).toBeVisible({ timeout: 2000 });

  const msgRow = page.getByText("Original content").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Edit").click();

  const textarea = page.locator("textarea").first();
  await expect(textarea).toBeVisible({ timeout: 2000 });
  await expect(textarea).toHaveValue("Original content");
});

// ── Cancel edit ──────────────────────────────────────────────────────────

test("pressing Escape cancels edit", async ({ page }) => {
  const msg = makeMessage({
    ID: "ec-1",
    Role: "user",
    Parts: [{ type: "text", Text: "Cancel me" }],
  });
  await setupWithMessage(page, msg);

  const msgRow = page.getByText("Cancel me").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Edit").click();

  const textarea = page.locator("textarea").first();
  await expect(textarea).toBeVisible({ timeout: 2000 });
  await textarea.fill("Changed text");
  await textarea.press("Escape");

  // Edit textarea gone (chat input textarea remains)
  await expect(msgRow.locator("textarea")).not.toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Cancel me")).toBeVisible();
});

test("clicking Cancel button cancels edit", async ({ page }) => {
  const msg = makeMessage({
    ID: "ec-2",
    Role: "assistant",
    // See the "edit button appears on hover for assistant message" test
    // above for why a terminal Finish part is required.
    Parts: [
      { type: "text", Text: "Cancel assistant" },
      { type: "finish", Reason: "end_turn", Message: "", Details: "" },
    ],
  });
  await setupWithMessage(page, msg);

  const msgRow = page.getByText("Cancel assistant").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Edit").click();

  await expect(msgRow.locator("textarea")).toBeVisible({ timeout: 2000 });
  await msgRow.locator("button", { hasText: "Cancel" }).click();

  await expect(msgRow.locator("textarea")).not.toBeVisible({ timeout: 2000 });
});

// ── Commit edit ─────────────────────────────────────────────────────────

test("clicking Save commits edit and sends update_message_content", async ({ page }) => {
  const msg = makeMessage({
    ID: "sv-1",
    Role: "user",
    Parts: [{ type: "text", Text: "Before save" }],
  });
  await setupWithMessage(page, msg);

  const msgRow = page.getByText("Before save").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Edit").click();

  const textarea = page.locator("textarea").first();
  await textarea.fill("After save");
  // msgRow locator is stale after fill (text changed), find Save button directly
  await page.locator("button").filter({ hasText: /^Save$/ }).click();

  const cmd = await waitForWSSend(page, "update_message_content");
  const payload = cmd.payload as { messageID: string; content: string };
  expect(payload.messageID).toBe("sv-1");
  expect(payload.content).toBe("After save");
});

test("Ctrl+Enter commits edit for assistant message", async ({ page }) => {
  const msg = makeMessage({
    ID: "ce-1",
    Role: "assistant",
    // See the "edit button appears on hover for assistant message" test
    // above for why a terminal Finish part is required.
    Parts: [
      { type: "text", Text: "Before ctrl-enter" },
      { type: "finish", Reason: "end_turn", Message: "", Details: "" },
    ],
  });
  await setupWithMessage(page, msg);

  const msgRow = page.getByText("Before ctrl-enter").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Edit").click();

  const textarea = page.locator("textarea").first();
  await textarea.fill("After ctrl-enter");
  await textarea.press("Control+Enter");

  const cmd = await waitForWSSend(page, "update_message_content");
  const payload = cmd.payload as { messageID: string; content: string };
  expect(payload.messageID).toBe("ce-1");
  expect(payload.content).toBe("After ctrl-enter");
});

// ── No-op edits ─────────────────────────────────────────────────────────

test("unchanged content does not send WS command", async ({ page }) => {
  const msg = makeMessage({
    ID: "noop-1",
    Role: "user",
    Parts: [{ type: "text", Text: "Same text" }],
  });
  await setupWithMessage(page, msg);

  const msgRow = page.getByText("Same text").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await msgRow.getByTitle("Edit").click();

  // Don't change the text, just click Save
  await msgRow.locator("button", { hasText: "Save" }).click();

  // Verify no update_message_content was sent
  const sent = await page.evaluate(() => {
    const s = ((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>;
    return s.some((m) => m.type === "update_message_content");
  });
  expect(sent).toBe(false);
});
