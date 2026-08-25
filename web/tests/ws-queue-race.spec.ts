import { test, expect, Page } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession } from "./helpers/fixtures";

// Regression coverage for task #726 (review 2026-08-25 P2): the
// agent_busy=false queue flush used to dequeueAllMessages() FIRST and then
// bare-ws.send() — when the socket died in between (the browser can
// dispatch that final frame after the socket already left OPEN), the queue
// was emptied and the message silently lost. Reconnect also used to
// blindly clear $busySessions: when a disconnect swallowed the last
// agent_busy=false of a turn that ended while offline, nothing would ever
// flush the local queue. Now the flush parks through sendQueued (outbox →
// delivered on reconnect), reconnect keeps busy only for sessions with
// queued work until the server's authoritative agent_busy replay decides,
// and an inject parked offline is re-shaped into a plain send_message.

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

// Kill the /ws socket from the inside, exactly like a network blip.
async function goOffline(page: Page) {
  await page.evaluate(() => {
    const mock = ((window as unknown) as Record<string, unknown>)["__mockWS"] as { close: () => void } | null;
    if (!mock) throw new Error("mock WS not created yet");
    mock.close();
  });
  await expect(page.getByTestId("chat-input-offline-indicator")).toBeVisible({ timeout: 5000 });
}

// Wait for the ~1s auto-reconnect to bring a live mock socket back.
async function waitForReconnect(page: Page) {
  await expect(page.getByTestId("chat-input-offline-indicator")).toBeHidden({ timeout: 15_000 });
}

// Deliver a server frame through the DEAD mock — exactly the browser race
// this task fixes: readyState is already CLOSED, but a message the server
// sent before the close is still dispatched. sendMockWSMessage cannot be
// used here because it (rightly) waits for readyState OPEN.
async function deliverThroughDeadSocket(page: Page, msg: unknown) {
  await page.evaluate((data: string) => {
    const mock = ((window as unknown) as Record<string, unknown>)["__mockWS"] as
      | { onmessage: ((ev: MessageEvent) => void) | null }
      | null;
    if (!mock || !mock.onmessage) throw new Error("dead mock has no onmessage");
    mock.onmessage(new MessageEvent("message", { data }));
  }, JSON.stringify(msg));
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

// ── Race 1: agent_busy=false lands while the socket is dying ────────────

test("queue flush racing a dead socket parks the message instead of losing it", async ({ page }) => {
  await openSession(page, "race-flush", "Race Flush");
  await setBusy(page, "race-flush", true);

  // Queue one message locally (the send button says "Queue" while busy).
  await page.getByTestId("chat-input-textarea").fill("flush me on reconnect");
  await page.getByTestId("chat-input-send-button").click();
  await expect(page.getByTestId("chat-input-textarea")).toHaveValue("");

  await goOffline(page);

  // The turn ends exactly as the connection dies: the final frame is
  // dispatched through a socket that is no longer writable. The old code
  // drained the queue here and dropped the content forever.
  await deliverThroughDeadSocket(page, {
    type: "agent_busy",
    payload: { SessionID: "race-flush", Busy: false },
  });

  // The drained queue was parked in the offline outbox, not lost.
  await expect(page.getByTestId("chat-input-offline-indicator")).toContainText("(1 queued)");

  // Auto-reconnect flushes the outbox: the queued text arrives after all.
  const msg = await waitForWSSend(page, "send_message", 15_000);
  expect(payloadOf(msg).content).toBe("flush me on reconnect");
});

// ── Race 2: reconnect re-syncs queued/busy state via the replay ─────────

test("reconnect keeps a queued session busy until the authoritative replay flushes it", async ({ page }) => {
  await openSession(page, "race-resync", "Race Resync");
  await setBusy(page, "race-resync", true);

  await page.getByTestId("chat-input-textarea").fill("queued across the outage");
  await page.getByTestId("chat-input-send-button").click();
  await expect(page.getByTestId("chat-input-textarea")).toHaveValue("");
  await expect(page.getByText("Queue · 1")).toBeVisible();

  // The turn finishes server-side while we are offline — its
  // agent_busy=false is swallowed by the dead socket and never delivered.
  await goOffline(page);
  await waitForReconnect(page);

  // Reconnect must NOT blindly clear busy for a session with queued work:
  // until the server's authoritative replay says otherwise, the composer
  // stays in Queue mode (the old code flipped to direct-send here).
  await expect(page.getByTestId("chat-input-send-button")).toContainText("Queue");
  expect(await framesOfType(page, "send_message")).toHaveLength(0);

  // The server's per-session authoritative replay (handleListSessions sends
  // one agent_busy after every sessions_list): the turn ended while offline.
  await sendMockWSMessage(page, {
    type: "agent_busy",
    payload: { SessionID: "race-resync", Busy: false },
  });

  // Busy clears and the queue flushes as a fresh send_message.
  await expect(page.getByTestId("chat-input-send-button")).not.toContainText("Queue");
  const msg = await waitForWSSend(page, "send_message", 5_000);
  expect(payloadOf(msg).content).toBe("queued across the outage");
  await expect(page.getByText("Queue · 1")).toBeHidden();
});

// ── Stale-action policy: inject parked offline becomes send_message ─────

test("inject parked during a disconnect is delivered as a fresh send_message", async ({ page }) => {
  await openSession(page, "race-inject", "Race Inject");
  await setBusy(page, "race-inject", true);

  await page.getByTestId("chat-input-textarea").fill("inject while offline");
  await goOffline(page);
  await page.getByTestId("chat-input-inject-button").click();

  // Parked (composer cleared honestly) and re-shaped per the stale-action
  // policy: an inject whose target turn may no longer exist at delivery
  // time goes out as a plain send_message, never as inject_message.
  await expect(page.getByTestId("chat-input-textarea")).toHaveValue("");
  await expect(page.getByTestId("chat-input-offline-indicator")).toContainText("(1 queued)");

  await waitForReconnect(page);
  const msg = await waitForWSSend(page, "send_message", 15_000);
  expect(payloadOf(msg).content).toBe("inject while offline");
  expect(await framesOfType(page, "inject_message")).toHaveLength(0);
});
