/**
 * Regression test for partIndex addressing in cross-message bursts.
 *
 * Guards against a bug where burst position (idx) was used for server
 * addressing instead of the real partIndex. The bug caused:
 *   - Delete operations to hit the wrong part (e.g., finish instead of thinking)
 *   - Edit operations to corrupt tool_call Input fields
 *
 * buildRenderItems merges tool_call/tool_result/thinking parts from N
 * consecutive messages into one "burst". ToolRun indexed burst entries by
 * BURST POSITION, but ToolActivityGroup/ActionRow used that position for
 * WS commands (update_message_part/delete_message_part). The REAL index of
 * the part inside its own message's Parts array is different (thinking is
 * usually Parts[0]), so the UI mis-addressed parts.
 *
 * Regression: F-1 round 23
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend, clearWSSent } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

const F = { type: "finish", Reason: "end_turn", Message: "", Details: "" };

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
    payload: [makeSession({ ID: "pidx-sess", Title: "PartIndex Session" })],
  });
  await expect(page.getByText("PartIndex Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("PartIndex Session").first().click();
  // Pin SessionID to "pidx-sess" for all messages (see comment in thinking-parts.spec.ts)
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: messages.map((m) => ({ ...m, SessionID: "pidx-sess" })),
  });
}

// ── Test A: Delete thinking in cross-message burst ─────────────────────────

test("thinking-row delete inside a cross-message burst addresses the real part", async ({ page }) => {
  // Burst: [a1.call, t1.result, a2.thinking, a2.call]
  // a2's thinking is burst-position 2 but real index 0 (Parts[0] in message a2)
  await setupWithMessages(page, [
    makeMessage({ ID: "u1", Role: "user", Parts: [{ type: "text", Text: "go" }] }),
    makeMessage({
      ID: "a1",
      Role: "assistant",
      Parts: [
        { type: "tool_call", ID: "tc-a1", Name: "bash", Input: '{"command":"ls"}', Finished: true },
        F,
      ],
    }),
    makeMessage({
      ID: "t1",
      Role: "tool",
      Parts: [{ type: "tool_result", ToolCallID: "tc-a1", Name: "bash", Content: "ok", IsError: false }],
    }),
    makeMessage({
      ID: "a2",
      Role: "assistant",
      Parts: [
        { type: "thinking", Thinking: "now edit it" },
        { type: "tool_call", ID: "tc-a2", Name: "edit", Input: '{"file_path":"x"}', Finished: true },
        F,
      ],
    }),
  ]);

  await clearWSSent(page);

  // Find the thinking row with text "now edit it" and delete it
  const row = page.locator('[data-test-id="action-row"]').filter({ hasText: "now edit it" });
  await row.hover();
  await row.getByTitle("Delete thinking").click();
  await page.locator(".modal-panel button", { hasText: "Delete" }).click();

  // The delete_message_part command must use the REAL partIndex (0 in message a2)
  const cmd = await waitForWSSend(page, "delete_message_part");
  expect(cmd.payload).toEqual({ messageID: "a2", partIndex: 0 });
});

// ── Test B: Second thinking row in a burst ─────────────────────────────────

test("second thinking row in a burst addresses the real part", async ({ page }) => {
  // b2 thinking row is burst-position 3 (out of range for b2's parts array)
  // Real index is 0 (Parts[0] in message b2)
  await setupWithMessages(page, [
    makeMessage({ ID: "u1", Role: "user", Parts: [{ type: "text", Text: "go" }] }),
    makeMessage({
      ID: "b1",
      Role: "assistant",
      Parts: [
        { type: "thinking", Thinking: "first look" },
        { type: "tool_call", ID: "tc-b1", Name: "bash", Input: '{"command":"pwd"}', Finished: true },
        F,
      ],
    }),
    makeMessage({
      ID: "tb1",
      Role: "tool",
      Parts: [{ type: "tool_result", ToolCallID: "tc-b1", Name: "bash", Content: "ok", IsError: false }],
    }),
    makeMessage({
      ID: "b2",
      Role: "assistant",
      Parts: [
        { type: "thinking", Thinking: "second look" },
        { type: "tool_call", ID: "tc-b2", Name: "bash", Input: '{"command":"cd"}', Finished: true },
        F,
      ],
    }),
  ]);

  await clearWSSent(page);

  // Find the thinking row with "second look", expand it first (EditForm only renders when expanded)
  const row = page.locator('[data-test-id="action-row"]').filter({ hasText: "second look" });
  await row.locator('[data-test-id="action-row-toggle"]').click();
  await row.hover();
  await row.getByTitle("Edit thinking").click();
  await row.locator("textarea").fill("EDITED REASONING");
  await row.locator("button", { hasText: "Save" }).click();

  // The update_message_part command must use the REAL partIndex (0 in message b2)
  const cmd = await waitForWSSend(page, "update_message_part");
  expect(cmd.payload).toEqual({ messageID: "b2", partIndex: 0, content: "EDITED REASONING" });
});

// ── Test C: Thinking row flushed by a narrating text ───────────────────────

test("thinking row flushed by a narrating text still addresses the real part", async ({ page }) => {
  // c2's thinking joins the first burst (burst-position 2 = the tool_call that would be corrupted)
  // Real index is 0 (Parts[0] in message c2)
  // The first group auto-collapses (a newer renderitem follows)
  await setupWithMessages(page, [
    makeMessage({ ID: "u1", Role: "user", Parts: [{ type: "text", Text: "go" }] }),
    makeMessage({
      ID: "c1",
      Role: "assistant",
      Parts: [
        { type: "tool_call", ID: "tc-c1", Name: "view", Input: '{"file_path":"a.ts"}', Finished: true },
        F,
      ],
    }),
    makeMessage({
      ID: "tc1",
      Role: "tool",
      Parts: [{ type: "tool_result", ToolCallID: "tc-c1", Name: "view", Content: "ok", IsError: false }],
    }),
    makeMessage({
      ID: "c2",
      Role: "assistant",
      Parts: [
        { type: "thinking", Thinking: "the file exists" },
        { type: "text", Text: "OK, the file exists, now let me edit it" },
        { type: "tool_call", ID: "tc-c2", Name: "edit", Input: '{"file_path":"a.ts"}', Finished: true },
        F,
      ],
    }),
  ]);

  await clearWSSent(page);

  // First group auto-collapses — expand it via the toggle
  const toggle = page.locator('[data-test-id="tool-activity-toggle"]').first();
  if ((await toggle.getAttribute("aria-expanded")) === "false") {
    await toggle.click();
  }

  // Find the thinking row with "the file exists", expand and edit it
  const row = page.locator('[data-test-id="action-row"]').filter({ hasText: "the file exists" });
  await row.locator('[data-test-id="action-row-toggle"]').click();
  await row.hover();
  await row.getByTitle("Edit thinking").click();
  await row.locator("textarea").fill("EDITED C");
  await row.locator("button", { hasText: "Save" }).click();

  // The update_message_part command must use the REAL partIndex (0 in message c2)
  const cmd = await waitForWSSend(page, "update_message_part");
  expect(cmd.payload).toEqual({ messageID: "c2", partIndex: 0, content: "EDITED C" });
});
