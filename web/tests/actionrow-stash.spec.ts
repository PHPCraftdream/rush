/**
 * Regression test for the burst thinking row's edit-form stash.
 *
 * ActionRow's burst thinking rows keep an `editingThinking` flag that swaps
 * the row body for an EditForm. Before the fix, the row's own toggle only
 * flipped the open override, so collapsing mid-edit left the flag set: a
 * later expand (to READ the reasoning) ambushed the operator with a stale
 * edit textarea instead — the abandoned draft was gone too, and Cancel was
 * required just to see the content. The fix routes the row's toggle through
 * a `collapse` that also clears `editingThinking`, mirroring ThinkingPart's
 * 55d32c4d safeguard (whose standalone "Thoughts" card behavior is asserted
 * here as the control).
 *
 * Regression: F-4 round 25
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
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
    payload: [makeSession({ ID: "stash-sess", Title: "Stash Session" })],
  });
  await expect(page.getByText("Stash Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Stash Session").first().click();
  // Pin SessionID to "stash-sess" for all messages (see comment in thinking-parts.spec.ts)
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: messages.map((m) => ({ ...m, SessionID: "stash-sess" })),
  });
}

// Burst: [d1.thinking, d1.call] + td1.result + [d2.thinking, d2.call].
// Only the LAST action in the burst defaults open, so the first thinking
// row genuinely starts collapsed — the edit-then-collapse sequence below
// exercises the row's own toggle, not an auto-open.
const BURST = [
  makeMessage({ ID: "u1", Role: "user", Parts: [{ type: "text", Text: "go" }] }),
  makeMessage({
    ID: "d1",
    Role: "assistant",
    Parts: [
      { type: "thinking", Thinking: "ROW-ONE-REASONING" },
      { type: "tool_call", ID: "tc-d1", Name: "bash", Input: '{"command":"pwd"}', Finished: true },
      F,
    ],
  }),
  makeMessage({
    ID: "td1",
    Role: "tool",
    Parts: [{ type: "tool_result", ToolCallID: "tc-d1", Name: "bash", Content: "ok", IsError: false }],
  }),
  makeMessage({
    ID: "d2",
    Role: "assistant",
    Parts: [
      { type: "thinking", Thinking: "ROW-TWO-REASONING" },
      { type: "tool_call", ID: "tc-d2", Name: "bash", Input: '{"command":"cd"}', Finished: true },
      F,
    ],
  }),
];

// ── Test A: collapsing a burst thinking row mid-edit does not stash the form

test("collapse via the row's own toggle clears the edit form; re-expand shows the reasoning", async ({ page }) => {
  await setupWithMessages(page, BURST);

  const row = page.locator('[data-test-id="action-row"]').filter({ hasText: "ROW-ONE-REASONING" });
  await expect(row).toBeVisible();
  const toggle = row.locator('[data-test-id="action-row-toggle"]');
  await expect(toggle).toHaveAttribute("aria-expanded", "false");

  // 1. Edit → row auto-expands (2c761d8d) and shows the form.
  await row.hover();
  await row.getByTitle("Edit thinking").click();
  await expect(toggle).toHaveAttribute("aria-expanded", "true");
  await expect(row.locator("textarea")).toBeVisible();

  // 2. Operator collapses the row via its own chevron — WITHOUT pressing
  //    Cancel inside the form (changed their mind / wants to scan the list).
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-expanded", "false");
  await expect(row.locator("textarea")).toHaveCount(0);

  // 3. Later they expand the row again to READ the reasoning: the content
  //    must come back, not a stashed edit textarea.
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-expanded", "true");
  await expect(row.locator("pre")).toContainText("ROW-ONE-REASONING");
  await expect(row.locator("textarea")).toHaveCount(0);
});

// ── Test B: control — ThinkingPart's standalone card (55d32c4d) ────────────

test("control: ThinkingPart's standalone Thoughts card does not stash across a collapse", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "u1", Role: "user", Parts: [{ type: "text", Text: "go" }] }),
    makeMessage({
      ID: "s1",
      Role: "assistant",
      Parts: [
        { type: "thinking", Thinking: "CARD-REASONING" },
        { type: "text", Text: "Answer." },
        F,
      ],
    }),
  ]);

  const card = page.getByTestId("thinking-card");
  await expect(card).toBeVisible();
  await card.hover();
  await card.getByTitle("Edit thinking").click();
  await expect(card.locator("textarea")).toBeVisible();

  await page.getByTestId("thinking-toggle").click(); // collapse mid-edit
  await expect(card.locator("textarea")).toHaveCount(0);
  await page.getByTestId("thinking-toggle").click(); // re-expand to read

  await expect(page.getByTestId("thinking-content")).toBeVisible();
  await expect(card.locator("textarea")).toHaveCount(0);
});
