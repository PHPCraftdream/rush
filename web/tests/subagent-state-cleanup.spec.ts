import { test, expect, Page } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

// Regression coverage for task #727 (review 2026-08-25 P3): removeSession
// used to clear only the session row and its ws request/queue bookkeeping —
// $subAgentSessions, $subAgentMessages, delete tombstones and message block
// breaks for the removed session were never cleared, so mass-deleting
// delegated sub-agent sessions left their transcripts in memory for the
// tab's lifetime (and late events kept routing into state nobody reads).
// The sessions_list handler also reaps a sub-agent branch whose parent was
// deleted server-side while this tab was offline.

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

const PARENT_ID = "parent-cleanup";
const PARENT_MSG_ID = "pmsg-cleanup";
const SUB_1 = `${PARENT_MSG_ID}$$call-cleanup-1`;
const SUB_2 = `${PARENT_MSG_ID}$$call-cleanup-2`;

// Open the parent session and render one assistant message carrying two
// agent tool_calls — each mounts a SubAgentBlock keyed by
// `${messageID}$$${toolCallID}` (see SubAgentBlock.tsx / Part.tsx), and
// each block's lazy-load registers its composite sub session ID and asks
// for its transcript.
async function openParentWithTwoSubAgents(page: Page) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: PARENT_ID, Title: "Cleanup Parent" })],
  });
  await page.getByText("Cleanup Parent").first().click();
  await expect(page.getByTestId("chat-input-textarea")).toBeEnabled({ timeout: 5000 });

  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: {
      SessionID: PARENT_ID,
      Messages: [
        makeMessage({
          ID: "pmsg-user",
          SessionID: PARENT_ID,
          Role: "user",
          Parts: [{ type: "text", Text: "please run the sub agent" }],
        }),
        makeMessage({
          ID: PARENT_MSG_ID,
          SessionID: PARENT_ID,
          Role: "assistant",
          Parts: [
            { type: "tool_call", Name: "agent", ID: "call-cleanup-1", Input: "{\"prompt\":\"sub one\"}", Finished: true },
            { type: "tool_call", Name: "agent", ID: "call-cleanup-2", Input: "{\"prompt\":\"sub two\"}", Finished: true },
          ],
        }),
      ],
    },
  });

  // Wait for both lazy load_messages requests before replying so the
  // replies cannot race the $subAgentSessions registration.
  await page.waitForFunction(
    (ids: string[]) => {
      const sent = (((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string; payload?: { sessionID?: string } }>) ?? [];
      return ids.every((want) =>
        sent.some((m) => m.type === "load_messages" && m.payload?.sessionID === want),
      );
    },
    [SUB_1, SUB_2],
    { timeout: 5000 },
  );

  // Untagged replies (no id) take the back-compat path and are always
  // accepted; each routes to its sub-agent transcript by SessionID.
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: {
      SessionID: SUB_1,
      Messages: [
        makeMessage({
          ID: "sub1-msg",
          SessionID: SUB_1,
          Role: "assistant",
          Parts: [{ type: "text", Text: "sub-agent one transcript line" }],
        }),
      ],
    },
  });
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: {
      SessionID: SUB_2,
      Messages: [
        makeMessage({
          ID: "sub2-msg",
          SessionID: SUB_2,
          Role: "assistant",
          Parts: [{ type: "text", Text: "sub-agent two transcript line" }],
        }),
      ],
    },
  });

  await expect(page.getByText("sub-agent one transcript line")).toBeVisible({ timeout: 5000 });
  await expect(page.getByText("sub-agent two transcript line")).toBeVisible({ timeout: 5000 });
}

test("deleting a sub-agent session clears its transcript and spares siblings", async ({ page }) => {
  await openParentWithTwoSubAgents(page);

  // Mass sub-agent deletion (e.g. sessions purge): every removed id gets
  // its own session_deleted push funnelling through removeSession.
  await sendMockWSMessage(page, {
    type: "session_deleted",
    payload: { ID: SUB_1 },
  });

  // The removed sub-agent's transcript state is gone — its block falls
  // back to the empty rendering — while the untouched sibling keeps its
  // transcript and the parent transcript is unaffected.
  await expect(page.getByText("sub-agent one transcript line")).toBeHidden({ timeout: 5000 });
  await expect(page.getByText("sub-agent two transcript line")).toBeVisible();
  await expect(page.getByText("please run the sub agent")).toBeVisible();
});

test("parent deleted while offline reaps its sub-agent branch on the reconnect list", async ({ page }) => {
  await openParentWithTwoSubAgents(page);

  // Go offline (kill the mock socket), then deliver a fresh sessions_list
  // through the dead socket: the tab missed the cascade of
  // session_deleted pushes that ran on the server while it was away.
  await page.evaluate(() => {
    const mock = ((window as unknown) as Record<string, unknown>)["__mockWS"] as { close: () => void } | null;
    if (!mock) throw new Error("mock WS not created yet");
    mock.close();
  });
  await expect(page.getByTestId("chat-input-offline-indicator")).toBeVisible({ timeout: 5000 });

  await page.evaluate((data: string) => {
    const mock = ((window as unknown) as Record<string, unknown>)["__mockWS"] as
      | { onmessage: ((ev: MessageEvent) => void) | null }
      | null;
    if (!mock || !mock.onmessage) throw new Error("dead mock has no onmessage");
    mock.onmessage(new MessageEvent("message", { data }));
  }, JSON.stringify({ type: "sessions_list", payload: [] }));

  // An empty top-level list routes through the create_session early
  // return before any active-session switching, so the parent stays
  // active and its SubAgentBlocks stay mounted — making the reaped
  // transcripts directly observable.
  await expect(page.getByText("sub-agent one transcript line")).toBeHidden({ timeout: 5000 });
  await expect(page.getByText("sub-agent two transcript line")).toBeHidden();
  await expect(page.getByText("please run the sub agent")).toBeVisible();
});
