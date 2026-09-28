import { test, expect, Page } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession } from "./helpers/fixtures";

// Coverage for task #1057: per-queued-message "Send now" (inject_message,
// merges into the running turn) and "Interrupt & send" (interrupt_and_send,
// cancels the turn and starts a new one) buttons.
//
// The core contract under test: takeQueuedMessage (store_commands.ts) removes
// the item from $messageQueue SYNCHRONOUSLY, before the wsRequest round-trip
// starts -- so the item can never be picked up a second time by the
// agent_busy=false flush (dequeueAllMessages in useWS.ts), no matter how the
// two race. A failed send restores the item to its original queue position
// with a visible error.

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

async function openSession(page: Page, id: string, title: string) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: id, Title: title })],
  });
  await expect(page.getByText(title).first()).toBeVisible({ timeout: 5000 });
  await page.getByText(title).first().click();
  await expect(page.getByTestId("chat-input-textarea")).toBeEnabled({ timeout: 5000 });
}

async function setBusy(page: Page, sessionID: string, busy: boolean) {
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: sessionID, Busy: busy },
  });
}

// Queues one message the way a busy composer does: fill + click the
// "Queue" send button (agentBusy makes send() call enqueueMessage instead
// of sending immediately -- see ChatInput.tsx's send()).
async function queueMessage(page: Page, text: string) {
  await page.getByTestId("chat-input-textarea").fill(text);
  await page.getByTestId("chat-input-send-button").click();
  await expect(page.getByTestId("chat-input-textarea")).toHaveValue("");
}

function payloadOf(msg: { payload?: unknown }): Record<string, unknown> {
  return (msg.payload ?? {}) as Record<string, unknown>;
}

async function framesOfType(page: Page, type: string) {
  return page.evaluate((t: string) => {
    const sent = (((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string; payload?: unknown }>) ?? [];
    return sent.filter((m) => m.type === t);
  }, type);
}

// ── Visibility: buttons only show while the session is busy ────────────────

test("send-now / interrupt-and-send buttons only show while the session is busy", async ({ page }) => {
  await openSession(page, "vis-1", "Visibility Session");
  await setBusy(page, "vis-1", true);
  await queueMessage(page, "queued while busy");
  await expect(page.getByTestId("queued-message-send-now")).toBeVisible();
  await expect(page.getByTestId("queued-message-interrupt-send")).toBeVisible();

  // Busy flipping false drains the queue via the normal flush -- the whole
  // row (and with it the buttons) disappears.
  await setBusy(page, "vis-1", false);
  await waitForWSSend(page, "send_message");
  await expect(page.getByTestId("queued-message-send-now")).toBeHidden();
  await expect(page.getByTestId("queued-message-interrupt-send")).toBeHidden();
});

// A message restored by a FAILED send-now can land back in the queue after
// the session already went idle -- e.g. the turn legitimately ended right
// before the error reply arrived. The actions must stay hidden for it: they
// only make sense against a running turn.
test("a message restored after the session already went idle has its actions hidden", async ({ page }) => {
  await openSession(page, "vis-2", "Race Visibility Session");
  await setBusy(page, "vis-2", true);
  await queueMessage(page, "a");
  await queueMessage(page, "b");

  await page.getByTestId("queued-message-send-now").nth(0).click();
  const cmd = await waitForWSSend(page, "inject_message");
  expect(payloadOf(cmd).content).toBe("a");

  // The turn ends (and "b", the only thing still queued, is flushed) before
  // the reply to "a"'s send-now attempt comes back.
  await setBusy(page, "vis-2", false);
  await waitForWSSend(page, "send_message");

  await sendMockWSMessage(page, { type: "error", id: cmd.id, error: "turn already ended" });

  await expect(page.getByText("Queue · 1")).toBeVisible();
  await expect(page.getByTestId("queued-message-error")).toBeVisible();
  await expect(page.getByTestId("queued-message-send-now")).toBeHidden();
  await expect(page.getByTestId("queued-message-interrupt-send")).toBeHidden();
});

// ── Send now: inject_message, atomic removal, no double-send on flush ──────

test("send now sends via inject_message, removes only that item, and it never goes out again on flush", async ({ page }) => {
  await openSession(page, "now-1", "Send Now Session");
  await setBusy(page, "now-1", true);
  await queueMessage(page, "first message");
  await queueMessage(page, "second message");
  await expect(page.getByText("Queue · 2")).toBeVisible();

  await page.getByTestId("queued-message-send-now").first().click();

  const cmd = await waitForWSSend(page, "inject_message");
  expect(payloadOf(cmd).sessionID).toBe("now-1");
  expect(payloadOf(cmd).content).toBe("first message");

  // Removed the instant it was claimed -- before any reply arrived.
  await expect(page.getByText("Queue · 1")).toBeVisible();
  await expect(page.getByText("first message")).toBeHidden();
  await expect(page.getByText("second message")).toBeVisible();

  await sendMockWSMessage(page, { type: "response", id: cmd.id, payload: { status: "injected" } });

  // The turn is still running (inject doesn't end it); the remaining item
  // stays queued until the turn actually finishes.
  await expect(page.getByText("Queue · 1")).toBeVisible();

  await setBusy(page, "now-1", false);
  const flushed = await waitForWSSend(page, "send_message");
  // Only the still-queued message goes out -- "first message" must never
  // reappear on this or any later frame.
  expect(payloadOf(flushed).content).toBe("second message");
  expect(await framesOfType(page, "inject_message")).toHaveLength(1);
});

// ── Interrupt & send: interrupt_and_send, same atomicity contract ──────────

test("interrupt & send sends via interrupt_and_send and removes only that item", async ({ page }) => {
  await openSession(page, "int-1", "Interrupt Session");
  await setBusy(page, "int-1", true);
  await queueMessage(page, "alpha");
  await queueMessage(page, "beta");
  await expect(page.getByText("Queue · 2")).toBeVisible();

  // Act on the SECOND item -- proves order/position addressing, not just
  // "the first one in the DOM".
  await page.getByTestId("queued-message-interrupt-send").nth(1).click();

  const cmd = await waitForWSSend(page, "interrupt_and_send");
  expect(payloadOf(cmd).content).toBe("beta");

  await expect(page.getByText("Queue · 1")).toBeVisible();
  await expect(page.getByText("alpha")).toBeVisible();
  await expect(page.getByText("beta")).toBeHidden();

  await sendMockWSMessage(page, { type: "response", id: cmd.id, payload: { status: "queued" } });
  await expect(page.getByText("Queue · 1")).toBeVisible();

  await setBusy(page, "int-1", false);
  const flushed = await waitForWSSend(page, "send_message");
  expect(payloadOf(flushed).content).toBe("alpha");
  expect(await framesOfType(page, "interrupt_and_send")).toHaveLength(1);
});

// ── Failure: the message returns to its original position with an error ───

test("a failed send-now restores the message to its original queue position with a visible error", async ({ page }) => {
  await openSession(page, "fail-1", "Failure Session");
  await setBusy(page, "fail-1", true);
  await queueMessage(page, "one");
  await queueMessage(page, "two");
  await queueMessage(page, "three");

  // Act on the middle item.
  await page.getByTestId("queued-message-send-now").nth(1).click();
  const cmd = await waitForWSSend(page, "inject_message");
  expect(payloadOf(cmd).content).toBe("two");
  await expect(page.getByText("Queue · 2")).toBeVisible();

  await sendMockWSMessage(page, { type: "error", id: cmd.id, error: "session busy elsewhere" });

  // Restored, error visible, and back at its ORIGINAL index (between "one"
  // and "three"), not appended at the end.
  await expect(page.getByText("Queue · 3")).toBeVisible();
  await expect(page.getByTestId("queued-message-error")).toContainText("session busy elsewhere");
  const bubbles = page.locator(".group\\/qi");
  await expect(bubbles).toHaveCount(3);
  await expect(bubbles.nth(0)).toContainText("one");
  await expect(bubbles.nth(1)).toContainText("two");
  await expect(bubbles.nth(2)).toContainText("three");

  // Nothing was double-sent: exactly one inject_message frame went out for
  // the failed attempt, and the eventual flush carries all three texts
  // (the restored message is a normal queued message again).
  expect(await framesOfType(page, "inject_message")).toHaveLength(1);
  await setBusy(page, "fail-1", false);
  const flushed = await waitForWSSend(page, "send_message");
  expect(payloadOf(flushed).content).toBe("one\n\ntwo\n\nthree");
});

// ── An already-dead socket fails immediately and restores the message ──────

test("send now with an already-dead socket fails fast and restores the message", async ({ page }) => {
  await openSession(page, "dead-1", "Dead Socket Session");
  await setBusy(page, "dead-1", true);
  await queueMessage(page, "only message");

  await page.evaluate(() => {
    const mock = ((window as unknown) as Record<string, unknown>)["__mockWS"] as { close: () => void } | null;
    if (!mock) throw new Error("mock WS not created yet");
    mock.close();
  });

  await page.getByTestId("queued-message-send-now").click();

  await expect(page.getByTestId("queued-message-error")).toContainText("Not connected", { timeout: 3000 });
  await expect(page.getByText("Queue · 1")).toBeVisible();
  expect(await framesOfType(page, "inject_message")).toHaveLength(0);
});
