import { test, expect, Page } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend, clearWSSent } from "./helpers/mock-ws";
import { makeConfig, makeSession } from "./helpers/fixtures";

// Regression coverage for review F3 (P2): an attachment-only draft (no typed
// text) used to be unsendable — send/sendFast early-returned on empty
// text.trim() and canSend disabled every send button, so clicking Send did
// nothing, with zero feedback. The backend's send_message accepts
// Attachments independent of Content (it appends "[Attached file: <path>]"
// itself), so the composer now sends attachment-only frames with empty
// content.
//
// Slash-command decision under test: command handling is text-only by
// construction — an attachment-only send has no text, so it must bypass
// handleSitterCommand and reach the wire untouched.

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

const ONE_PNG = [{ fileName: "screenshot.png", mimeType: "image/png", data: b64("hello") }];

// ── canSend: buttons react to an attachment alone ──────────────────────────

test("send button is enabled by an attachment alone; whitespace text alone is not", async ({ page }) => {
  await openSession(page, "ao-enable", "Attach Only Enable");
  await expect(page.getByTestId("chat-input-send-button")).toBeDisabled();

  // Whitespace-only text is still "no text".
  await page.getByTestId("chat-input-textarea").fill("   ");
  await expect(page.getByTestId("chat-input-send-button")).toBeDisabled();

  await dropFile(page, "screenshot.png", "hello");
  await expect(page.getByTestId("chat-input-send-button")).toBeEnabled();
});

// ── Idle send path ─────────────────────────────────────────────────────────

test("attachment-only send fires send_message with empty content and the attachment", async ({ page }) => {
  await openSession(page, "ao-idle", "Attach Only Idle");
  await dropFile(page, "screenshot.png", "hello");
  await page.getByTestId("chat-input-send-button").click();

  const msg = await waitForWSSend(page, "send_message");
  expect(payloadOf(msg).content ?? "").toBe("");
  expect(attachmentsOf(msg)).toEqual(ONE_PNG);
  // Badge clears because the attachment genuinely went out.
  await expect(page.getByText("screenshot.png")).toBeHidden();
});

test("attachment-only Enter-to-send reaches the wire too", async ({ page }) => {
  await openSession(page, "ao-enter", "Attach Only Enter");
  await dropFile(page, "screenshot.png", "hello");
  await page.getByTestId("chat-input-textarea").press("Enter");

  const msg = await waitForWSSend(page, "send_message");
  expect(payloadOf(msg).content ?? "").toBe("");
  expect(attachmentsOf(msg)).toEqual(ONE_PNG);
});

// ── Fast-send path ─────────────────────────────────────────────────────────

test("attachment-only fast-send carries the model override and attachment", async ({ page }) => {
  await openSession(page, "ao-fast", "Attach Only Fast");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await dropFile(page, "screenshot.png", "hello");
  await page.locator('button[title="Send with lightweight model"]').click();

  const msg = await waitForWSSend(page, "send_message");
  const payload = payloadOf(msg);
  expect(payload.content ?? "").toBe("");
  expect(payload.smartModel).toEqual({ provider: "anthropic", model: "claude-haiku-4" });
  expect(attachmentsOf(msg)).toEqual(ONE_PNG);
  await expect(page.getByText("screenshot.png")).toBeHidden();
});

// ── Queue-while-busy path ──────────────────────────────────────────────────

test("attachment-only queue-while-busy flushes with its attachment", async ({ page }) => {
  await openSession(page, "ao-q", "Attach Only Queue");
  await setBusy(page, "ao-q", true);
  await dropFile(page, "screenshot.png", "hello");
  await page.getByTestId("chat-input-send-button").click();
  await expect(page.getByText("screenshot.png")).toBeHidden();

  await clearWSSent(page);
  await setBusy(page, "ao-q", false);
  const msg = await waitForWSSend(page, "send_message");
  expect(payloadOf(msg).content ?? "").toBe("");
  expect(attachmentsOf(msg)).toEqual(ONE_PNG);
});

// ── Interrupt / Inject (busy) — enabled by canSend, must not be dead buttons ─

test("attachment-only interrupt fires interrupt_and_send with the attachment", async ({ page }) => {
  await openSession(page, "ao-int", "Attach Only Interrupt");
  await setBusy(page, "ao-int", true);
  await dropFile(page, "screenshot.png", "hello");
  await page.getByTestId("chat-input-interrupt-button").click();

  const msg = await waitForWSSend(page, "interrupt_and_send");
  expect(payloadOf(msg).content ?? "").toBe("");
  expect(attachmentsOf(msg)).toEqual(ONE_PNG);
  await expect(page.getByText("screenshot.png")).toBeHidden();
});

test("attachment-only inject fires inject_message with the attachment", async ({ page }) => {
  await openSession(page, "ao-inj", "Attach Only Inject");
  await setBusy(page, "ao-inj", true);
  await dropFile(page, "screenshot.png", "hello");
  await page.getByTestId("chat-input-inject-button").click();

  const msg = await waitForWSSend(page, "inject_message");
  expect(payloadOf(msg).content ?? "").toBe("");
  expect(attachmentsOf(msg)).toEqual(ONE_PNG);
  await expect(page.getByText("screenshot.png")).toBeHidden();
});

// ── Empty composer still does nothing ──────────────────────────────────────

test("empty composer keeps the send button disabled and Enter sends nothing", async ({ page }) => {
  await openSession(page, "ao-empty", "Attach Only Empty");
  await expect(page.getByTestId("chat-input-send-button")).toBeDisabled();
  await page.getByTestId("chat-input-textarea").press("Enter");
  await page.waitForTimeout(250);
  // The app sends housekeeping frames on connect — only assert that no
  // send_message went out.
  const sentTypes = await page.evaluate(() => {
    const sent = ((window as unknown) as Record<string, unknown>)["__wsSent"] as { type: string }[] | undefined;
    return (sent ?? []).map((m) => m.type);
  });
  expect(sentTypes).not.toContain("send_message");
});
