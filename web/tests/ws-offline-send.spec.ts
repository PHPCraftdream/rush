import { test, expect, Page } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession } from "./helpers/fixtures";

// Regression coverage for review F4 (P1): a send landing while the /ws
// socket is down used to vanish silently — ws.send() no-opped and
// ChatInput cleared the composer anyway. Now the composer only clears when
// the frame was written or parked in the offline outbox, the strip above
// the input announces the outage, and parked frames flush in FIFO order
// on reconnect.

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

// Drop a file onto the composer and wait for its badge.
async function dropFile(page: Page, name: string, contents: string) {
  await page.evaluate(({ name, contents }) => {
    const dropArea = document.querySelector("div[class*='rounded-2xl'][class*='bg-base-overlay']");
    if (!dropArea) throw new Error("composer drop area not found");
    const file = new File([contents], name, { type: "image/png" });
    const dt = new DataTransfer();
    dt.items.add(file);
    dropArea.dispatchEvent(new DragEvent("drop", { dataTransfer: dt, bubbles: true }));
  }, { name, contents });
  await expect(page.getByText(name).first()).toBeVisible({ timeout: 5000 });
}

// Kill the /ws socket from the inside, exactly like a network blip: the
// mock's close() runs the real onclose path, so the client drops the
// socket, flips $connected off and schedules its normal ~1s reconnect.
// Resolve the composer work (typing, attachments, busy flags) BEFORE
// calling this — server→client pushes are impossible while offline.
async function goOffline(page: Page) {
  await page.evaluate(() => {
    const mock = ((window as unknown) as Record<string, unknown>)["__mockWS"] as { close: () => void } | null;
    if (!mock) throw new Error("mock WS not created yet");
    mock.close();
  });
  await expect(page.getByTestId("chat-input-offline-indicator")).toBeVisible({ timeout: 5000 });
}

async function framesOfType(page: Page, type: string) {
  return page.evaluate((t: string) => {
    const sent = (((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string; payload?: unknown }>) ?? [];
    return sent.filter((m) => m.type === t);
  }, type);
}

function payloadOf(msg: { payload?: unknown }): Record<string, unknown> {
  return (msg.payload ?? {}) as Record<string, unknown>;
}

// ── Idle send: held while offline, delivered on reconnect ──────────────────

test("idle send during a disconnect is held in the outbox and delivered on reconnect", async ({ page }) => {
  await openSession(page, "off-idle", "Offline Idle");
  await dropFile(page, "held.png", "hello");
  await page.getByTestId("chat-input-textarea").fill("held message");

  await goOffline(page);
  await page.getByTestId("chat-input-send-button").click();

  // The frame did NOT reach the wire while offline…
  expect(await framesOfType(page, "send_message")).toHaveLength(0);
  // …but the composer was still cleared honestly: the message is parked
  // and counted on the offline strip, not vanished.
  await expect(page.getByTestId("chat-input-textarea")).toHaveValue("");
  await expect(page.getByText("held.png")).toBeHidden();
  await expect(page.getByTestId("chat-input-offline-indicator")).toContainText("1 queued");

  // The ~1s auto-reconnect flushes the outbox with content + attachment.
  const msg = await waitForWSSend(page, "send_message", 15_000);
  expect(payloadOf(msg).content).toBe("held message");
  expect((payloadOf(msg).attachments as { fileName: string }[]).map((a) => a.fileName)).toEqual(["held.png"]);
  await expect(page.getByTestId("chat-input-offline-indicator")).toBeHidden({ timeout: 5000 });
});

// ── Fast-send: draft must survive a dead socket ────────────────────────────

test("fast-send during a disconnect keeps the draft instead of dropping it", async ({ page }) => {
  await openSession(page, "off-fast", "Offline Fast");
  await page.getByTestId("chat-input-textarea").fill("fast draft");

  await goOffline(page);
  await page.locator('button[title="Send with lightweight model"]').click();

  // Nothing sent, nothing cleared: the draft stays editable.
  await expect(page.getByTestId("chat-input-textarea")).toHaveValue("fast draft");
  expect(await framesOfType(page, "send_message")).toHaveLength(0);

  // Even after the socket comes back the frame must not appear from nowhere.
  await expect(page.getByTestId("chat-input-offline-indicator")).toBeHidden({ timeout: 5000 });
  await page.waitForTimeout(300);
  expect(await framesOfType(page, "send_message")).toHaveLength(0);
  await expect(page.getByTestId("chat-input-textarea")).toHaveValue("fast draft");
});

// ── Interrupt: held while offline, delivered on reconnect ───────────────────

test("interrupt during a disconnect is held and delivered on reconnect", async ({ page }) => {
  await openSession(page, "off-int", "Offline Interrupt");
  await setBusy(page, "off-int", true);
  await page.getByTestId("chat-input-textarea").fill("interrupt me");

  await goOffline(page);
  await page.getByTestId("chat-input-interrupt-button").click();

  expect(await framesOfType(page, "interrupt_and_send")).toHaveLength(0);
  await expect(page.getByTestId("chat-input-textarea")).toHaveValue("");

  const msg = await waitForWSSend(page, "interrupt_and_send", 15_000);
  expect(payloadOf(msg).content).toBe("interrupt me");
});

// ── FIFO order across multiple offline sends ────────────────────────────────

test("offline sends flush in FIFO order", async ({ page }) => {
  await openSession(page, "off-fifo", "Offline FIFO");

  await goOffline(page);
  await page.getByTestId("chat-input-textarea").fill("first offline");
  await page.getByTestId("chat-input-send-button").click();
  await page.getByTestId("chat-input-textarea").fill("second offline");
  await page.getByTestId("chat-input-send-button").click();

  await expect(page.getByTestId("chat-input-offline-indicator")).toContainText("2 queued");

  await waitForWSSend(page, "send_message", 15_000);
  const frames = await framesOfType(page, "send_message");
  expect(frames.map((m) => payloadOf(m).content)).toEqual(["first offline", "second offline"]);
});
