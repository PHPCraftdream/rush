/**
 * Regression coverage for release-readiness audit round 7, finding F9.
 *
 * A provider can emit a tool_call whose Input is not an object-shaped JSON
 * payload — a bare "null", a bare number ("42"), or an array ("[]"). All are
 * valid JSON, so JSON.parse succeeds and the TypeScript cast adds no runtime
 * guard: the formatter then indexed parsed[k] (bash-shaped tools) or called
 * Object.values(parsed) (unknown tools) on a null/number and threw a
 * TypeError OUTSIDE the try/catch. Rendered from SubAgentBlock's tool rows
 * that exception escapes the render and React unmounts the tree, blanking
 * the whole sub-agent transcript.
 *
 * The fix treats every non-object payload as "no structured input" inside
 * formatActionArgs: a bare JSON string previews itself, everything else
 * (null, number, array) shows nothing. Nothing throws, and the rest of the
 * transcript — sibling rows and assistant prose — must still render.
 */

import { test, expect, Page } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

const PARENT_ID = "parent-nullfmt";
const PARENT_MSG_ID = "pmsg-nullfmt";
const AGENT_CALL = "call-nullfmt";
const SUB_ID = `${PARENT_MSG_ID}$$${AGENT_CALL}`;

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// Same mount choreography as subagent-block-metadata.spec.ts: open the
// parent session, mount one agent tool_call's SubAgentBlock, wait for its
// lazy load_messages, then deliver the sub-agent transcript.
async function openSubAgentBlock(
  page: Page,
  prompt: string,
  transcript: ReturnType<typeof makeMessage>[],
) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: PARENT_ID, Title: "Null Input Parent" })],
  });
  await page.getByText("Null Input Parent").first().click();
  await expect(page.getByTestId("chat-input-textarea")).toBeEnabled({ timeout: 5000 });

  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: {
      SessionID: PARENT_ID,
      Messages: [
        makeMessage({
          ID: "pmsg-user-nullfmt",
          SessionID: PARENT_ID,
          Role: "user",
          Parts: [{ type: "text", Text: "delegate please" }],
        }),
        makeMessage({
          ID: PARENT_MSG_ID,
          SessionID: PARENT_ID,
          Role: "assistant",
          Parts: [
            { type: "tool_call", Name: "agent", ID: AGENT_CALL, Input: JSON.stringify({ prompt }), Finished: true },
          ],
        }),
      ],
    },
  });

  await page.waitForFunction(
    (want: string) => {
      const sent = (((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string; payload?: { sessionID?: string } }>) ?? [];
      return sent.some((m) => m.type === "load_messages" && m.payload?.sessionID === want);
    },
    SUB_ID,
    { timeout: 5000 },
  );

  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: { SessionID: SUB_ID, Messages: transcript },
  });

  return page.locator(".sub-agent-block").filter({ hasText: prompt });
}

test("non-object tool input (null, 42, []) renders no preview and never blanks the transcript", async ({ page }) => {
  const block = await openSubAgentBlock(page, "null input probe", [
    makeMessage({
      ID: "sub-nullfmt-1",
      SessionID: SUB_ID,
      Role: "assistant",
      // parsed[k] on null is the throw the guard removes.
      Parts: [
        { type: "tool_call", ID: "tc-null", Name: "bash", Input: "null", Finished: true },
        { type: "text", Text: "TRANSCRIPT-STILL-HERE" },
      ],
      CreatedAt: 1700000000,
    }),
    makeMessage({
      ID: "sub-nullfmt-2",
      SessionID: SUB_ID,
      Role: "assistant",
      // Object.values on a bare number is the unknown-tool throw.
      Parts: [
        { type: "tool_call", ID: "tc-number", Name: "mystery_tool", Input: "42", Finished: true },
      ],
      CreatedAt: 1700000001,
    }),
    makeMessage({
      ID: "sub-nullfmt-3",
      SessionID: SUB_ID,
      Role: "assistant",
      // Array: never threw, but is unstructured all the same. The bare JSON
      // string exercises the string-preview fallback; the healthy object
      // row is the control.
      Parts: [
        { type: "tool_call", ID: "tc-array", Name: "bash", Input: "[]", Finished: true },
        { type: "tool_call", ID: "tc-string", Name: "mystery_tool", Input: JSON.stringify("raw string payload"), Finished: true },
        { type: "tool_call", ID: "tc-ok", Name: "bash", Input: JSON.stringify({ command: "echo healthy" }), Finished: true },
      ],
      CreatedAt: 1700000002,
    }),
  ]);

  // A thrown render error unmounts the React root (no error boundary), so
  // "the rest of the transcript is still visible" is the proof the
  // formatter did not throw on any non-object payload above.
  await expect(block.getByText("TRANSCRIPT-STILL-HERE")).toBeVisible({ timeout: 5000 });
  await expect(block.getByText("echo healthy")).toBeVisible();

  // The bare JSON string previews itself (the safe scalar fallback), and
  // every malformed row still renders its tool name: 3 bash rows
  // (null / [] / healthy object) and 2 mystery_tool rows (42 / string).
  await expect(block.getByText("raw string payload")).toBeVisible();
  await expect(block.locator("span.text-mauve").filter({ hasText: /^bash$/ })).toHaveCount(3);
  await expect(block.locator("span.text-mauve").filter({ hasText: "mystery_tool" })).toHaveCount(2);
});
