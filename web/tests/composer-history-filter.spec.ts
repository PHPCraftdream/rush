/**
 * Web composer history pollution — the frontend half of the fix (#1055/#1056).
 *
 * The composer's ArrowUp recall list and history dropdown are derived from
 * the session's message rows ($myPrompts in web/src/store.ts). They must
 * contain ONLY prompts a human typed in the web composer. Eligibility is
 * gated by ONE shared predicate, isComposerTypedMessage, built on a SINGLE
 * server-computed field: Message.HumanTyped (internal/server/wire.go's
 * isHumanTyped) — computed from structured fields ONLY (role, Hidden,
 * IsSummaryMessage, NoticeKind, AutoResumed, BackgroundJobNotice, Origin),
 * never from message text.
 *
 * Before the #1056 fix, the client filtered on the individual flags
 * (BackgroundJobNotice, AutoResumed, Origin) directly and never looked at
 * NoticeKind at all. That missed every notice kind whose ONLY structured
 * marker is NoticeKind — in production this is exactly the shape of a
 * supervision check-in and a one-time timeout check-in, both persisted via
 * a plain context.Background() (no AutoResumed, no BackgroundJobNotice, no
 * Origin). This spec proves the gap with fixtures set to the worst case for
 * the OLD filter: Origin: "web" and no other flags, so ONLY the NoticeKind
 * check can exclude them — reading the pre-fix $myPrompts body (BackgroundJob
 * Notice/AutoResumed/Origin checks only, no NoticeKind) confirms rows u-
 * notice-supervision / u-notice-wake-failed / u-notice-timeout / u-notice-
 * via-inject below would have passed straight through into recall.
 *
 * Regression: web composer history pollution (four-session-bugs, #1055).
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

const SESSION_ID = "composer-sess";
const RECALL_BTN = 'button[title="Click to put this prompt back in the input"]';

// Creation-chronological transcript covering every reported pollution class
// plus every genuinely-typed channel. HumanTyped is set explicitly on each
// row exactly as the fixed server (toMessageWire/isHumanTyped) would compute
// it, so this spec exercises the CLIENT half of the fix in isolation.
const TRANSCRIPT = [
  makeMessage({
    ID: "u-web-1", SessionID: SESSION_ID, Role: "user", Origin: "web", HumanTyped: true,
    Parts: [{ type: "text", Text: "first web prompt" }],
  }),
  makeMessage({
    ID: "a-1", SessionID: SESSION_ID, Role: "assistant",
    Parts: [
      { type: "text", Text: "done" },
      { type: "finish", Reason: "end_turn", Message: "", Details: "" },
    ],
  }),
  // Genuinely typed, but NOT in the web composer — rush run.
  makeMessage({
    ID: "u-cli-1", SessionID: SESSION_ID, Role: "user", Origin: "cli", HumanTyped: false,
    Parts: [{ type: "text", Text: "cli run prompt" }],
  }),
  // Async job completion notice (coordinator.notifyAsyncCompletion): always
  // carries BOTH AutoResumed and BackgroundJobNotice, and Origin "web".
  makeMessage({
    ID: "u-notice-async", SessionID: SESSION_ID, Role: "user", Origin: "web",
    AutoResumed: true, BackgroundJobNotice: true, NoticeKind: "", HumanTyped: false,
    Parts: [{ type: "text", Text: "Async job call-1 (bash) finished.\n\ndone" }],
  }),
  // Background shell notice, Phase 3 fallback (notifyBackgroundJobDone):
  // BackgroundJobNotice only, no Origin (ctx never tags one on this path).
  makeMessage({
    ID: "u-notice-bgshell", SessionID: SESSION_ID, Role: "user", Origin: "",
    AutoResumed: false, BackgroundJobNotice: true, NoticeKind: "", HumanTyped: false,
    Parts: [{ type: "text", Text: "Background job shell-1 (`npm run build`) finished: exit 0, ran 4s.\n\nok" }],
  }),
  // Phase 4 autonomous idle-resume turn (notifyBackgroundJobDone's autonomy
  // branch): both flags true, Origin "web".
  makeMessage({
    ID: "u-notice-autoresume", SessionID: SESSION_ID, Role: "user", Origin: "web",
    AutoResumed: true, BackgroundJobNotice: true, NoticeKind: "", HumanTyped: false,
    Parts: [{ type: "text", Text: "Background job shell-2 (`npm test`) finished: exit 0, ran 9s.\n\nok" }],
  }),
  // Supervision check-in (supervision.go): NoticeKind is its ONLY marker in
  // production (context.Background() -> no AutoResumed/BackgroundJobNotice/
  // Origin). Origin is set to "web" here deliberately — the worst case for
  // the pre-fix filter, which never looked at NoticeKind.
  makeMessage({
    ID: "u-notice-supervision", SessionID: SESSION_ID, Role: "user", Origin: "web",
    AutoResumed: false, BackgroundJobNotice: false, NoticeKind: "supervision", HumanTyped: false,
    Parts: [{ type: "text", Text: "Still working — check-in #1. No progress reported yet." }],
  }),
  // wake_failed marker (coordinator_wake.go's persistWakeFailedMarker):
  // NoticeKind is again the only reliable marker in the worst case.
  makeMessage({
    ID: "u-notice-wake-failed", SessionID: SESSION_ID, Role: "user", Origin: "web",
    AutoResumed: false, BackgroundJobNotice: false, NoticeKind: "wake_failed", HumanTyped: false,
    Parts: [{ type: "text", Text: "Failed to resume after event call-2: connection reset. Event saved; will resume next turn." }],
  }),
  // One-time timeout check-in (work_ledger_timeout.go's timeoutWakeOnly):
  // same shape as supervision — NoticeKind only.
  makeMessage({
    ID: "u-notice-timeout", SessionID: SESSION_ID, Role: "user", Origin: "web",
    AutoResumed: false, BackgroundJobNotice: false, NoticeKind: "timeout_wake_only", HumanTyped: false,
    Parts: [{ type: "text", Text: "Timeout reached for async job call-3 (bash) — it is still running." }],
  }),
  // A notice delivered via wakeSession's InjectMessage persist step (as
  // opposed to the Run/wake step) — proves exclusion is NOT about which
  // function persisted the row, only about the structured markers it carries.
  makeMessage({
    ID: "u-notice-via-inject", SessionID: SESSION_ID, Role: "user", Origin: "web",
    AutoResumed: false, BackgroundJobNotice: false, NoticeKind: "job_stopped", HumanTyped: false,
    Parts: [{ type: "text", Text: "Async job call-4 (bash) was stopped (job_kill). Partial output before the stop:\n\npartial" }],
  }),
  // Human web inject (ChatInput's "inject" / "interrupt & send") — travels
  // through the SAME InjectMessage call as the notice above, but carries NO
  // notice markers, so it is genuinely composer-typed and must stay.
  makeMessage({
    ID: "u-web-inject", SessionID: SESSION_ID, Role: "user", Origin: "web", HumanTyped: true,
    Parts: [{ type: "text", Text: "injected: also check the retry path" }],
  }),
  makeMessage({
    ID: "u-web-2", SessionID: SESSION_ID, Role: "user", Origin: "web", HumanTyped: true,
    Parts: [{ type: "text", Text: "second web prompt" }],
  }),
];

// The ONLY rows in TRANSCRIPT that must ever reach recall, oldest first.
const EXPECTED_TYPED = ["first web prompt", "injected: also check the retry path", "second web prompt"];

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

test("history dropdown lists only human-typed web prompts, newest first", async ({ page }) => {
  await openSessionWith(page, TRANSCRIPT);

  await page.getByTestId("chat-input-history-button").click();
  const recallTexts = await page.locator(RECALL_BTN).allInnerTexts();
  expect(recallTexts.map((t) => t.trim())).toEqual([...EXPECTED_TYPED].reverse());
});

test("ArrowUp cycles only human-typed web prompts in order, skipping every notice kind", async ({ page }) => {
  await openSessionWith(page, TRANSCRIPT);

  const textarea = page.getByTestId("chat-input-textarea");
  await textarea.click();

  // Walk all the way up: newest typed prompt first, then older ones, never a
  // notice or the CLI prompt sitting between them in the transcript.
  // Unrolled (not a loop) to keep each ArrowUp/ArrowDown step's own await
  // sequential and explicit, matching EXPECTED_TYPED's fixed 3 entries.
  await textarea.press("ArrowUp");
  await expect(textarea).toHaveValue(EXPECTED_TYPED[2]);
  await textarea.press("ArrowUp");
  await expect(textarea).toHaveValue(EXPECTED_TYPED[1]);
  await textarea.press("ArrowUp");
  await expect(textarea).toHaveValue(EXPECTED_TYPED[0]);
  // Clamped at the oldest typed prompt — must NOT walk into the CLI prompt
  // or any of the eight notice rows that sit around it in the transcript.
  await textarea.press("ArrowUp");
  await expect(textarea).toHaveValue(EXPECTED_TYPED[0]);

  // ArrowDown walks back toward the live draft.
  await textarea.press("ArrowDown");
  await expect(textarea).toHaveValue(EXPECTED_TYPED[1]);
  await textarea.press("ArrowDown");
  await expect(textarea).toHaveValue(EXPECTED_TYPED[2]);
  await textarea.press("ArrowDown");
  await expect(textarea).toHaveValue("");
});

test("empty history when no human-typed web prompts exist", async ({ page }) => {
  await openSessionWith(page, [
    makeMessage({ ID: "u-cli-only", SessionID: SESSION_ID, Role: "user", Origin: "cli", HumanTyped: false, Parts: [{ type: "text", Text: "cli run prompt" }] }),
    makeMessage({
      ID: "u-notice-only", SessionID: SESSION_ID, Role: "user", Origin: "web",
      BackgroundJobNotice: true, HumanTyped: false,
      Parts: [{ type: "text", Text: "Async job call-9 (bash) finished.\n\ndone" }],
    }),
    makeMessage({
      ID: "u-supervision-only", SessionID: SESSION_ID, Role: "user", Origin: "web",
      NoticeKind: "supervision", HumanTyped: false,
      Parts: [{ type: "text", Text: "Still working — check-in #1." }],
    }),
  ]);

  // No recall entries → the history button is disabled and ArrowUp is inert.
  await expect(page.getByTestId("chat-input-history-button")).toBeDisabled();
  const textarea = page.getByTestId("chat-input-textarea");
  await textarea.click();
  await textarea.press("ArrowUp");
  await expect(textarea).toHaveValue("");
});
