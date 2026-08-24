import { test, expect, Page } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend, clearWSSent } from "./helpers/mock-ws";
import { makeConfig, makeSession } from "./helpers/fixtures";

// Regression coverage for review F-1: attachments must ride on every send
// path, not just the idle Send. The idle test is the control that brackets
// the two defect paths (fast-send, queue-while-busy) the way the review's
// own harness did.

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

// Drop a file onto the composer (works whether the agent is busy or not)
// and wait for its badge to appear.
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

function b64(s: string): string {
  return Buffer.from(s, "utf8").toString("base64");
}

function payloadOf(msg: { payload?: unknown }): Record<string, unknown> {
  return (msg.payload ?? {}) as Record<string, unknown>;
}

function attachmentsOf(msg: { payload?: unknown }): { fileName: string; mimeType: string; data: string }[] {
  return ((payloadOf(msg).attachments ?? []) as { fileName: string; mimeType: string; data: string }[]);
}

// ── Idle path (control — was already correct) ──────────────────────────────

test("idle send carries attachments in the send_message frame (control)", async ({ page }) => {
  await openSession(page, "att-idle", "Attach Idle");
  await dropFile(page, "screenshot.png", "hello");
  await page.getByTestId("chat-input-textarea").fill("look at this");
  await page.getByTestId("chat-input-send-button").click();

  const msg = await waitForWSSend(page, "send_message");
  const payload = payloadOf(msg);
  expect(payload.content).toBe("look at this");
  expect(attachmentsOf(msg)).toEqual([
    { fileName: "screenshot.png", mimeType: "image/png", data: b64("hello") },
  ]);
  // Badge clears only because the attachment actually went out.
  await expect(page.getByText("screenshot.png")).toBeHidden();
});

// ── Fast-send path ──────────────────────────────────────────────────────────

test("fast-send carries attachments alongside the model override", async ({ page }) => {
  await openSession(page, "att-fast", "Attach Fast");
  // Config provides models.fast, so the frame must ALSO keep smartModel.
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await dropFile(page, "screenshot.png", "hello");
  await page.getByTestId("chat-input-textarea").fill("what is in this image?");
  await page.locator('button[title="Send with lightweight model"]').click();

  const msg = await waitForWSSend(page, "send_message");
  const payload = payloadOf(msg);
  expect(payload.smartModel).toEqual({ provider: "anthropic", model: "claude-haiku-4" });
  expect(attachmentsOf(msg)).toEqual([
    { fileName: "screenshot.png", mimeType: "image/png", data: b64("hello") },
  ]);
  // Badge clearing is honest now: the attachment is genuinely in the frame.
  await expect(page.getByText("screenshot.png")).toBeHidden();
});

// ── Queue-while-busy path ───────────────────────────────────────────────────

test("queued send carries its attachment on the flushed send", async ({ page }) => {
  await openSession(page, "att-q", "Attach Queue");
  await dropFile(page, "screenshot.png", "hello");
  await setBusy(page, "att-q", true);
  await page.getByTestId("chat-input-textarea").fill("look at this");
  await page.getByTestId("chat-input-send-button").click();
  // The badge clears at queue time: the attachment now belongs to the
  // queued message and will ride on the flush.
  await expect(page.getByText("screenshot.png")).toBeHidden();

  await clearWSSent(page);
  await setBusy(page, "att-q", false);
  const msg = await waitForWSSend(page, "send_message");
  const payload = payloadOf(msg);
  expect(payload.content).toBe("look at this");
  expect(attachmentsOf(msg)).toEqual([
    { fileName: "screenshot.png", mimeType: "image/png", data: b64("hello") },
  ]);
});

test("flush concatenates attachments from multiple queued messages in queue order", async ({ page }) => {
  await openSession(page, "att-q2", "Attach Queue2");
  await setBusy(page, "att-q2", true);

  await dropFile(page, "a.png", "hello");
  await page.getByTestId("chat-input-textarea").fill("first");
  await page.getByTestId("chat-input-send-button").click();
  await expect(page.getByText("a.png")).toBeHidden();

  await dropFile(page, "b.png", "world");
  await page.getByTestId("chat-input-textarea").fill("second");
  await page.getByTestId("chat-input-send-button").click();
  await expect(page.getByText("b.png")).toBeHidden();

  await clearWSSent(page);
  await setBusy(page, "att-q2", false);
  const msg = await waitForWSSend(page, "send_message");
  const payload = payloadOf(msg);
  expect(payload.content).toBe("first\n\nsecond");
  expect(attachmentsOf(msg).map((a) => a.fileName)).toEqual(["a.png", "b.png"]);
  expect(attachmentsOf(msg).map((a) => a.data)).toEqual([b64("hello"), b64("world")]);
});

// ── Interrupt / Inject fold queued attachments ──────────────────────────────

test("interrupt folds queued attachments into interrupt_and_send", async ({ page }) => {
  await openSession(page, "att-int", "Attach Interrupt");
  await setBusy(page, "att-int", true);

  await dropFile(page, "shot.png", "hello");
  await page.getByTestId("chat-input-textarea").fill("queued text");
  await page.getByTestId("chat-input-send-button").click();
  await expect(page.getByText("shot.png")).toBeHidden();

  await dropFile(page, "now.png", "world");
  await page.getByTestId("chat-input-textarea").fill("interrupt now");
  await page.getByTestId("chat-input-interrupt-button").click();

  const msg = await waitForWSSend(page, "interrupt_and_send");
  const payload = payloadOf(msg);
  expect(payload.content).toBe("queued text\n\ninterrupt now");
  expect(attachmentsOf(msg).map((a) => a.fileName)).toEqual(["shot.png", "now.png"]);
});

test("inject folds queued attachments into inject_message", async ({ page }) => {
  await openSession(page, "att-inj", "Attach Inject");
  await setBusy(page, "att-inj", true);

  await dropFile(page, "shot.png", "hello");
  await page.getByTestId("chat-input-textarea").fill("queued text");
  await page.getByTestId("chat-input-send-button").click();
  await expect(page.getByText("shot.png")).toBeHidden();

  await dropFile(page, "now.png", "world");
  await page.getByTestId("chat-input-textarea").fill("inject now");
  await page.getByTestId("chat-input-inject-button").click();

  const msg = await waitForWSSend(page, "inject_message");
  const payload = payloadOf(msg);
  expect(payload.content).toBe("queued text\n\ninject now");
  expect(attachmentsOf(msg).map((a) => a.fileName)).toEqual(["shot.png", "now.png"]);
});
