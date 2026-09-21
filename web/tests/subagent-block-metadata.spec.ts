/**
 * Regression coverage for commits 924add8a and 14dd4a54 on SubAgentBlock.
 *
 * - 924add8a: the block header shows the model + reasoning effort the
 *   sub-agent started with, derived from the FIRST assistant message of the
 *   sub-agent's own transcript that carries a non-empty Model/ReasoningEffort
 *   (model as plain text, effort via EffortBadge's [L]/[M]/[H]/... badge).
 * - 14dd4a54: each tool_call row inside the block renders the
 *   formatActionArgs() preview (the most useful argument for the tool name —
 *   "command" for bash) plus a TimeBadge for the owning message, instead of
 *   the bare tool name alone.
 */

import { test, expect, Page } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

const PARENT_ID = "parent-meta";
const PARENT_MSG_ID = "pmsg-meta";
const AGENT_CALL = "call-meta";
const SUB_ID = `${PARENT_MSG_ID}$$${AGENT_CALL}`;

// Same local-time formatting as TimeBadge; the test process and the browser
// share the machine's timezone, so the expected string can be computed here.
function expectedBadgeText(epochSec: number): string {
  const d = new Date(epochSec * 1000);
  return [d.getHours(), d.getMinutes(), d.getSeconds()]
    .map((n) => String(n).padStart(2, "0"))
    .join(":");
}

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// Open the parent session, mount one agent tool_call's SubAgentBlock, wait
// for its lazy load_messages, then deliver the given transcript. Returns the
// block locator scoped by the delegation prompt shown in its header.
async function openSubAgentBlock(
  page: Page,
  prompt: string,
  transcript: ReturnType<typeof makeMessage>[],
) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: PARENT_ID, Title: "SubAgent Meta Parent" })],
  });
  await page.getByText("SubAgent Meta Parent").first().click();
  await expect(page.getByTestId("chat-input-textarea")).toBeEnabled({ timeout: 5000 });

  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: {
      SessionID: PARENT_ID,
      Messages: [
        makeMessage({
          ID: "pmsg-user-meta",
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

  // The block registers its composite sub session and asks for the
  // transcript on mount; reply only once that request is visible so the
  // answer cannot race the $subAgentSessions registration.
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

test("sub-agent block header shows the first assistant message's model and effort", async ({ page }) => {
  const block = await openSubAgentBlock(page, "explore the repo", [
    makeMessage({
      ID: "sub-meta-1",
      SessionID: SUB_ID,
      Role: "assistant",
      Model: "gpt-5",
      ReasoningEffort: "high",
      Parts: [{ type: "text", Text: "first model line" }],
      CreatedAt: 1700000000,
    }),
    // A later assistant message (e.g. after a handoff) must NOT win the
    // header: the derivation takes the FIRST non-empty values.
    makeMessage({
      ID: "sub-meta-2",
      SessionID: SUB_ID,
      Role: "assistant",
      Model: "later-model-ignored",
      ReasoningEffort: "low",
      Parts: [{ type: "text", Text: "second model line" }],
      CreatedAt: 1700000050,
    }),
  ]);

  await expect(block.getByText("first model line")).toBeVisible({ timeout: 5000 });

  const header = block.locator(".sub-agent-toggle");
  await expect(header.getByText("gpt-5")).toBeVisible();
  const effort = header.locator('[title="Reasoning effort: high"]');
  await expect(effort).toHaveText("[H]");
  await expect(header.getByText("later-model-ignored")).toHaveCount(0);
  await expect(header.locator('[title="Reasoning effort: low"]')).toHaveCount(0);
});

test("sub-agent tool row shows the bash command preview and a timestamp", async ({ page }) => {
  const ROW_TS = 1700000090;
  const block = await openSubAgentBlock(page, "run the checks", [
    makeMessage({
      ID: "sub-row-1",
      SessionID: SUB_ID,
      Role: "assistant",
      Parts: [
        { type: "tool_call", ID: "sub-tc-bash", Name: "bash", Input: JSON.stringify({ command: "go test ./..." }), Finished: true },
      ],
      CreatedAt: ROW_TS,
    }),
  ]);

  // formatActionArgs("bash", ...) surfaces the command itself, not just the
  // bare tool name (14dd4a54).
  const preview = block.getByText("go test ./...");
  await expect(preview).toBeVisible({ timeout: 5000 });

  const row = preview.locator("xpath=..");
  await expect(row).toContainText("bash");
  await expect(row.locator("span.tabular-nums")).toHaveText(expectedBadgeText(ROW_TS));
});
