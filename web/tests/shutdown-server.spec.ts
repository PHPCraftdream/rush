import { test, expect } from "@playwright/test";
import { setupMockWS } from "./helpers/mock-ws";
import { sendMockWSMessage } from "./helpers/send-mock-ws-message";
import { waitForWSSend } from "./helpers/wait-for-ws-send";
import { fulfillAuthOk } from "./helpers/fulfill-auth-ok";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", fulfillAuthOk);
});

/** Browser-side: wraps the mock constructor to count every new /ws dial. */
function installDialCounter() {
  const w = window as unknown as Record<string, unknown>;
  const Orig = w.WebSocket as new (url: string, protocols?: string | string[]) => WebSocket;
  w.__wsDials = 0;
  /** Wrapper constructor counting every new /ws dial. */
  function CountingWebSocket(this: unknown, url: string, protocols?: string | string[]) {
    const urlStr = String(url);
    if (urlStr.includes("/ws") && !urlStr.includes("rsbuild")) {
      w.__wsDials = (w.__wsDials as number) + 1;
    }
    return new Orig(url, protocols);
  }
  CountingWebSocket.CONNECTING = 0;
  CountingWebSocket.OPEN = 1;
  CountingWebSocket.CLOSING = 2;
  CountingWebSocket.CLOSED = 3;
  CountingWebSocket.prototype = Orig.prototype;
  w.WebSocket = CountingWebSocket;
}

/** Browser-side: zeroes the /ws dial counter. */
function resetDialCounter() {
  (window as unknown as Record<string, unknown>)["__wsDials"] = 0;
}

/** Browser-side: closes the mock /ws socket via its real onclose path. */
function closeMockSocket() {
  const mock = ((window as unknown) as Record<string, unknown>)["__mockWS"] as { close: () => void } | null;
  if (!mock) throw new Error("mock WS not created yet");
  mock.close();
}

/** Browser-side: the number of /ws dials counted so far. */
function readDialCount() {
  return (window as unknown as Record<string, unknown>)["__wsDials"] as number;
}

test("confirm sends shutdown_server, acks into terminal state, and never reconnects", async ({ page }) => {
  await page.goto("/");

  // Count every NEW /ws dial from here on: wrap the selective mock constructor.
  await page.evaluate(installDialCounter);
  // NOTE: this counter also catches React StrictMode's double-mount initial
  // connects; it is re-zeroed below, right before the socket is killed, so
  // only post-shutdown dials are counted.

  await page.getByTestId("header-more-button").click();
  await page.getByTestId("header-shutdown-button").click();

  // Confirm dialog appears before anything is sent.
  await expect(page.getByText("Shut down server")).toBeVisible();

  // IMPORTANT ORDER: click the dialog's confirm button BEFORE waitForWSSend
  await page.getByRole("button", { name: "Shut down", exact: true }).click();
  const cmd = await waitForWSSend(page, "shutdown_server");
  expect(cmd.id).toBeTruthy(); // correlated request
  expect(cmd.payload).toBeUndefined(); // no payload

  // Mock the ack (do NOT actually kill anything).
  await sendMockWSMessage(page, { type: "response", id: cmd.id!, payload: { status: "shutting_down" } });
  await expect(page.getByTestId("server-shutting-down")).toBeVisible({ timeout: 5000 });
  await expect(page.getByText("Server is shutting down")).toBeVisible();

  // Dialog closed.
  await expect(page.getByText("Shut down server")).toBeHidden();

  // Re-zero the dial counter here: everything counted so far includes the
  // initial StrictMode double-mount connects, which are not reconnects.
  await page.evaluate(resetDialCounter);

  // Now the server-side process would die: kill the mock socket exactly like
  // the real close, and prove the client does NOT auto-reconnect and does NOT
  // show the transient banner.
  await page.evaluate(closeMockSocket);

  await expect(page.getByTestId("server-shutting-down")).toBeVisible();
  await expect(page.getByText("Reconnecting…", { exact: true })).toBeHidden();

  // Normal reconnect delay is ~1s (doubling). If suppression were broken a
  // new dial would appear within ~1-2s; wait past that window and assert zero.
  await page.waitForTimeout(3000);
  const dials = await page.evaluate(readDialCount);
  expect(dials).toBe(0);
});

test("error reply keeps the dialog open with an inline error and no terminal state", async ({ page }) => {
  await page.goto("/");

  await page.getByTestId("header-more-button").click();
  await page.getByTestId("header-shutdown-button").click();

  // Confirm dialog appears
  await expect(page.getByText("Shut down server")).toBeVisible();

  // Click confirm and wait for the command
  await page.getByRole("button", { name: "Shut down", exact: true }).click();
  const cmd = await waitForWSSend(page, "shutdown_server");

  // Mock an error reply
  await sendMockWSMessage(page, { type: "error", id: cmd.id!, error: "shutdown refused" });

  // Inline error should be visible in the dialog
  await expect(page.getByTestId("confirm-dialog-error")).toHaveText("shutdown refused", { timeout: 2000 });
  await expect(page.getByTestId("server-shutting-down")).toBeHidden();

  // Dialog stays mounted so the operator can retry or cancel.
  await expect(page.getByText("Shut down server")).toBeVisible();
});

/** Browser-side: every frame the page has sent. */
function readSentFrames() {
  return (((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>) ?? [];
}

/** True for a shutdown_server frame. */
function isShutdownServerFrame(m: { type: string }) {
  return m.type === "shutdown_server";
}

test("cancel sends nothing", async ({ page }) => {
  await page.goto("/");

  await page.getByTestId("header-more-button").click();
  await page.getByTestId("header-shutdown-button").click();

  // Confirm dialog appears
  await expect(page.getByText("Shut down server")).toBeVisible();

  // Click cancel
  await page.getByRole("button", { name: "Cancel", exact: true }).click();

  // Dialog should be hidden
  await expect(page.getByText("Shut down server")).toBeHidden();

  // Assert NO shutdown_server frame was ever sent
  const sent = await page.evaluate(readSentFrames);
  expect(sent.filter(isShutdownServerFrame)).toHaveLength(0);
});
