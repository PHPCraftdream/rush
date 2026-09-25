/**
 * Web composer history pollution — the frontend half of the fix.
 *
 * The composer's ArrowUp recall list and history dropdown are derived from
 * the session's message rows ($myPrompts in web/src/store.ts). They must
 * contain ONLY prompts a human typed in the web composer. Before the fix the
 * derivation ignored authorship, so these rows all surfaced under ArrowUp:
 *
 *   - CLI-originated prompts (`rush run`, `rush sessions inject`), carried
 *     with Origin: "cli" (message.OriginCLI → MessageWire.Origin);
 *   - async completion notices ("Async job ... finished"), which carry
 *     Origin: "web" AND BackgroundJobNotice: true — origin alone is NOT
 *     enough to exclude them;
 *   - Phase 4 autonomous idle-resume turns (AutoResumed: true).
 *
 * The backend half of the fix (MessageWire.Origin) is pinned in
 * internal/server/web_composer_history_test.go.
 *
 * Regression: web composer history pollution (four-session-bugs).
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

const SESSION_ID = "composer-sess";
const RECALL_BTN = 'button[title="Click to put this prompt back in the input"]';

// Creation-chronological transcript: two human web prompts with pollution
// rows interleaved. The notice deliberately carries Origin "web" (as the
// server stamps it) so the test proves the notice flag is load-bearing.
const TRANSCRIPT = [
  makeMessage({ ID: "u-web-1", SessionID: SESSION_ID, Role: "user", Origin: "web", Parts: [{ type: "text", Text: "first web prompt" }] }),
  makeMessage({
    ID: "a-1",
    SessionID: SESSION_ID,
    Role: "assistant",
    Parts: [
      { type: "text", Text: "done" },
      { type: "finish", Reason: "end_turn", Message: "", Details: "" },
    ],
  }),
  makeMessage({ ID: "u-cli-1", SessionID: SESSION_ID, Role: "user", Origin: "cli", Parts: [{ type: "text", Text: "cli run prompt" }] }),
  makeMessage({
    ID: "u-notice-1",
    SessionID: SESSION_ID,
    Role: "user",
    Origin: "web",
    BackgroundJobNotice: true,
    Parts: [{ type: "text", Text: "Async job call-1 (bash) finished.\n\ndone" }],
  }),
  makeMessage({ ID: "u-web-2", SessionID: SESSION_ID, Role: "user", Origin: "web", Parts: [{ type: "text", Text: "second web prompt" }] }),
];

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

async function openSessionWith(page: import("@playwright/test").Page, messages: ReturnType<typeof makeMessage>[]) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: SESSION_ID, Title: "Composer Session" })],
  });
  await expect(page.getByText("Composer Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Composer Session").first().click();
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: { SessionID: SESSION_ID, Messages: messages, Watermark: 0 },
  });
  // Wait for the transcript to land.
  await expect(page.getByText("first web prompt").or(page.getByText("cli run prompt")).first()).toBeVisible({ timeout: 3000 });
}

test("history dropdown lists only web-typed prompts, newest first", async ({ page }) => {
  await openSessionWith(page, TRANSCRIPT);

  await page.getByTestId("chat-input-history-button").click();
  const recallTexts = await page.locator(RECALL_BTN).allInnerTexts();
  expect(recallTexts.map((t) => t.trim())).toEqual(["second web prompt", "first web prompt"]);
});

test("ArrowUp cycles only web-typed prompts in order", async ({ page }) => {
  await openSessionWith(page, TRANSCRIPT);

  const textarea = page.getByTestId("chat-input-textarea");
  await textarea.click();
  await textarea.press("ArrowUp");
  await expect(textarea).toHaveValue("second web prompt");
  await textarea.press("ArrowUp");
  await expect(textarea).toHaveValue("first web prompt");
  // Clamped at the oldest web prompt — must NOT walk into the CLI prompt or
  // the async notice that sit between the two web prompts in the transcript.
  await textarea.press("ArrowUp");
  await expect(textarea).toHaveValue("first web prompt");
  // ArrowDown walks back toward the live draft (index 1 → 0 → draft).
  await textarea.press("ArrowDown");
  await expect(textarea).toHaveValue("second web prompt");
  await textarea.press("ArrowDown");
  await expect(textarea).toHaveValue("");
});

test("empty history when no web-typed prompts exist", async ({ page }) => {
  await openSessionWith(page, [
    makeMessage({ ID: "u-cli-only", SessionID: SESSION_ID, Role: "user", Origin: "cli", Parts: [{ type: "text", Text: "cli run prompt" }] }),
    makeMessage({
      ID: "u-notice-only",
      SessionID: SESSION_ID,
      Role: "user",
      Origin: "web",
      BackgroundJobNotice: true,
      Parts: [{ type: "text", Text: "Async job call-2 (bash) finished.\n\ndone" }],
    }),
  ]);

  // No recall entries → the history button is disabled and ArrowUp is inert.
  await expect(page.getByTestId("chat-input-history-button")).toBeDisabled();
  const textarea = page.getByTestId("chat-input-textarea");
  await textarea.click();
  await textarea.press("ArrowUp");
  await expect(textarea).toHaveValue("");
});
