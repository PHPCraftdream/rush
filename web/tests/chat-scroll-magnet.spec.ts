/**
 * Auto-scroll-to-bottom "magnet" tests.
 *
 * While streaming, every message update re-triggers scrollIntoView if the
 * view was at the bottom. Scrolling up must disengage that immediately --
 * otherwise rapid streaming updates race ahead of the position-based check
 * and snap the view back down before a manual scroll-up ever registers.
 */

import { test, expect } from "@playwright/test";
import { setupMockWS } from "./helpers/mock-ws";
import { sendMockWSMessage } from "./helpers/send-mock-ws-message";
import { makeSession } from "./helpers/make-session";
import { makeMessage } from "./helpers/make-message";
import { fulfillAuthOk } from "./helpers/fulfill-auth-ok";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", fulfillAuthOk);
});

/** Builds the i-th seed message alternating user/assistant. */
function seedMessage(_: unknown, i: number) {
  return makeMessage({
    ID: `scroll-seed-${i}`,
    SessionID: "scroll-sess",
    Role: i % 2 === 0 ? "user" : "assistant",
    Parts: [{ type: "text", Text: `seed message number ${i}` }],
  });
}

/** Opens a session seeded with enough messages to overflow and sit at the bottom. */
async function setupScrollableSession(page: import("@playwright/test").Page) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "scroll-sess", Title: "Scroll Session" })],
  });
  await expect(page.getByText("Scroll Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Scroll Session").first().click();

  // Enough messages to make the container overflow and start scrolled to
  // the bottom.
  const seed = Array.from({ length: 30 }, seedMessage);
  await sendMockWSMessage(page, { type: "messages_list", payload: seed });
  await expect(page.getByText("seed message number 29")).toBeVisible({ timeout: 2000 });
}

/** Browser-side: the scroll offset of the container. */
function readScrollTop(el: HTMLElement) {
  return el.scrollTop;
}

/** The current scroll offset of the chat container. */
async function scrollTop(page: import("@playwright/test").Page) {
  return page.locator('[data-test-id="chat-scroll-container"]').evaluate(readScrollTop);
}

/** Browser-side: true when the container is within 80px of its bottom. */
function isScrolledToBottom(el: HTMLElement) {
  return el.scrollHeight - el.scrollTop - el.clientHeight <= 80;
}

/** True when the chat container is scrolled to (near) its bottom. */
async function isAtBottom(page: import("@playwright/test").Page) {
  return page.locator('[data-test-id="chat-scroll-container"]').evaluate(isScrolledToBottom);
}

/** Serializes the i-th growing message_updated frame of the burst. */
function burstFrame(_: unknown, i: number) {
  return JSON.stringify({
    type: "message_updated",
    payload: makeMessage({
      ID: "scroll-seed-29",
      SessionID: "scroll-sess",
      Role: "assistant",
      Parts: [{ type: "text", Text: `seed message number 29 growing ${"x".repeat(i * 20)}` }],
    }),
  });
}

/** Browser-side: scrolls to the top, wheels up and delivers the burst in one synchronous block. */
function wheelUpThenDeliverBurst(burstData: readonly string[]) {
  const container = document.querySelector('[data-test-id="chat-scroll-container"]') as HTMLElement;
  container.scrollTop = 0; // immediate; the resulting "scroll" event is queued, not synchronous
  container.dispatchEvent(new WheelEvent("wheel", { deltaY: -600, bubbles: true, cancelable: true }));
  const ws = ((window as unknown) as Record<string, unknown>)["__mockWS"] as {
    onmessage: ((ev: MessageEvent) => void) | null,
  } | null;
  for (const data of burstData) {
    ws?.onmessage?.(new MessageEvent("message", { data }));
  }
}

test("scrolling up mid-stream stays put instead of snapping back to bottom", async ({ page }) => {
  await setupScrollableSession(page);
  expect(await isAtBottom(page)).toBe(true);

  // The real bug is a race between handleWheel's own synchronous deltaY
  // check and handleScroll's position-based check, which only runs once
  // the browser gets around to dispatching the (separately queued) "scroll"
  // event for a real position change. Isolate handleWheel's contribution
  // specifically: set scrollTop directly (an immediate position change
  // that does NOT synchronously fire "scroll" -- that event is queued for
  // a later task), then, in the SAME synchronous block, dispatch a wheel
  // event and the whole update burst. handleScroll cannot have run yet;
  // only handleWheel's own deltaY check can have disengaged the magnet by
  // the time the burst lands.
  const burst = Array.from({ length: 8 }, burstFrame);

  await page.evaluate(wheelUpThenDeliverBurst, burst);

  await expect(page.getByText(/growing x{140}/)).toBeVisible({ timeout: 2000 });
  const top = await scrollTop(page);
  expect(top).toBeLessThan(200);
});

/** Browser-side: scrolls the container to its bottom. */
function scrollToBottom(el: HTMLElement) {
  el.scrollTop = el.scrollHeight;
}

test("scrolling back down to the bottom re-engages the magnet", async ({ page }) => {
  await setupScrollableSession(page);

  const container = page.locator('[data-test-id="chat-scroll-container"]');
  await container.hover();
  await page.mouse.wheel(0, -600);
  // A real OS-level wheel scroll (unlike test 1's synthetic dispatchEvent,
  // which the app's own handleWheel applies synchronously) is applied by the
  // browser compositor and can land in `scrollTop` a frame or two after
  // page.mouse.wheel() resolves -- confirmed by instrumenting this exact
  // assertion: scrollTop was still unchanged immediately after the wheel
  // call and had settled to the expected value within 50ms. The magnet
  // itself disengages synchronously (handleWheel's deltaY<0 check runs
  // inside the real wheel event, before any compositor scroll lands), so
  // this is a test-side race, not a production one -- poll like the
  // symmetric scroll-back-down assertion a few lines below already does,
  // instead of reading scrollTop the instant the wheel call resolves.
  await expect.poll(() => isAtBottom(page)).toBe(false);

  // Scroll back down to the bottom -- handleScroll's position check must
  // flip isAtBottomRef back to true.
  await container.evaluate(scrollToBottom);
  await container.dispatchEvent("scroll");
  await expect.poll(() => isAtBottom(page)).toBe(true);

  await sendMockWSMessage(page, {
    type: "message_updated",
    payload: makeMessage({
      ID: "scroll-seed-29",
      SessionID: "scroll-sess",
      Role: "assistant",
      Parts: [{ type: "text", Text: "seed message number 29 final answer" }],
    }),
  });

  await expect(page.getByText("seed message number 29 final answer")).toBeVisible({ timeout: 2000 });
  expect(await isAtBottom(page)).toBe(true);
});
