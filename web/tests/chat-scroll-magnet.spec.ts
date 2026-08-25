/**
 * Auto-scroll-to-bottom "magnet" tests.
 *
 * While streaming, every message update re-triggers scrollIntoView if the
 * view was at the bottom. Scrolling up must disengage that immediately --
 * otherwise rapid streaming updates race ahead of the position-based check
 * and snap the view back down before a manual scroll-up ever registers.
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

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
  const seed = Array.from({ length: 30 }, (_, i) =>
    makeMessage({
      ID: `scroll-seed-${i}`,
      SessionID: "scroll-sess",
      Role: i % 2 === 0 ? "user" : "assistant",
      Parts: [{ type: "text", Text: `seed message number ${i}` }],
    })
  );
  await sendMockWSMessage(page, { type: "messages_list", payload: seed });
  await expect(page.getByText("seed message number 29")).toBeVisible({ timeout: 2000 });
}

async function scrollTop(page: import("@playwright/test").Page) {
  return page.locator('[data-test-id="chat-scroll-container"]').evaluate((el) => el.scrollTop);
}

async function isAtBottom(page: import("@playwright/test").Page) {
  return page.locator('[data-test-id="chat-scroll-container"]').evaluate((el) => {
    return el.scrollHeight - el.scrollTop - el.clientHeight <= 80;
  });
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
  const burst = Array.from({ length: 8 }, (_, i) => JSON.stringify({
    type: "message_updated",
    payload: makeMessage({
      ID: "scroll-seed-29",
      SessionID: "scroll-sess",
      Role: "assistant",
      Parts: [{ type: "text", Text: `seed message number 29 growing ${"x".repeat(i * 20)}` }],
    }),
  }));

  await page.evaluate((burstData: string[]) => {
    const container = document.querySelector('[data-test-id="chat-scroll-container"]') as HTMLElement;
    container.scrollTop = 0; // immediate; the resulting "scroll" event is queued, not synchronous
    container.dispatchEvent(new WheelEvent("wheel", { deltaY: -600, bubbles: true, cancelable: true }));
    const ws = ((window as unknown) as Record<string, unknown>)["__mockWS"] as { onmessage: ((ev: MessageEvent) => void) | null } | null;
    for (const data of burstData) {
      ws?.onmessage?.(new MessageEvent("message", { data }));
    }
  }, burst);

  await expect(page.getByText(/growing x{140}/)).toBeVisible({ timeout: 2000 });
  const top = await scrollTop(page);
  expect(top).toBeLessThan(200);
});

test("scrolling back down to the bottom re-engages the magnet", async ({ page }) => {
  await setupScrollableSession(page);

  const container = page.locator('[data-test-id="chat-scroll-container"]');
  await container.hover();
  await page.mouse.wheel(0, -600);
  expect(await isAtBottom(page)).toBe(false);

  // Scroll back down to the bottom -- handleScroll's position check must
  // flip isAtBottomRef back to true.
  await container.evaluate((el) => {
    el.scrollTop = el.scrollHeight;
  });
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
