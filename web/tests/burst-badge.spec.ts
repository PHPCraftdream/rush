/**
 * Regression test for model and effort badge on cross-message burst thinking rows.
 *
 * Guards against bug F-3 round 24 where Chat.tsx never passed Model/ReasoningEffort
 * to ToolActivityGroup, so burst thinking rows showed NO model name and NO effort badge.
 * The dominant rendering path (cross-message bursts) was completely missing these attributes.
 *
 * buildRenderItems merges tool_call/tool_result/thinking parts from N consecutive messages
 * into one "burst" rendered by ToolActivityGroup → ActionRow. The thinking-row header
 * should render the model name and an effort badge ([L]/[M]/[H]/[X]/[XX] for
 * low/medium/high/xhigh/max) from the source message of each thinking part.
 *
 * The fix threads Model/ReasoningEffort PER PART so a burst spanning a model change
 * attributes each row its own source message's values (hence the mixed-model test).
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

const F = { type: "finish", Reason: "end_turn", Message: "", Details: "" };

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

async function setupWithMessages(
  page: import("@playwright/test").Page,
  messages: ReturnType<typeof makeMessage>[]
) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "badge-sess", Title: "Badge Session" })],
  });
  await expect(page.getByText("Badge Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Badge Session").first().click();
  // Pin SessionID to "badge-sess" for all messages
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: messages.map((m) => ({ ...m, SessionID: "badge-sess" })),
  });
}

test("burst thinking row shows its own message's model and effort badge", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "u1", Role: "user", Parts: [{ type: "text", Text: "go" }] }),
    makeMessage({
      ID: "b1",
      Role: "assistant",
      Model: "glm-4.7",
      ReasoningEffort: "max",
      Parts: [
        { type: "thinking", Thinking: "plan the work first" },
        { type: "tool_call", ID: "tc-b1", Name: "view", Input: '{"file_path":"a.ts"}', Finished: true },
        F,
      ],
    }),
    makeMessage({
      ID: "tb1",
      Role: "tool",
      Parts: [{ type: "tool_result", ToolCallID: "tc-b1", Name: "view", Content: "ok", IsError: false }],
    }),
    makeMessage({
      ID: "b2",
      Role: "assistant",
      Model: "glm-4.7",
      ReasoningEffort: "max",
      Parts: [
        { type: "thinking", Thinking: "now verify output" },
        { type: "tool_call", ID: "tc-b2", Name: "bash", Input: '{"command":"ls"}', Finished: true },
        F,
      ],
    }),
  ]);

  // First thinking row: "plan the work first"
  const row1 = page.locator('[data-test-id="action-row"]').filter({ hasText: "plan the work first" });
  await expect(row1).toBeVisible();
  await expect(row1.getByText("glm-4.7", { exact: true })).toBeVisible();
  const badge1 = row1.locator("span[title='Reasoning effort: max']");
  await expect(badge1).toBeVisible();
  await expect(badge1).toHaveText("[XX]");

  // Second thinking row: "now verify output"
  const row2 = page.locator('[data-test-id="action-row"]').filter({ hasText: "now verify output" });
  await expect(row2).toBeVisible();
  await expect(row2.getByText("glm-4.7", { exact: true })).toBeVisible();
  const badge2 = row2.locator("span[title='Reasoning effort: max']");
  await expect(badge2).toBeVisible();
  await expect(badge2).toHaveText("[XX]");
});

test("burst spanning two different models attributes each thinking row its own model/effort", async ({ page }) => {
  await setupWithMessages(page, [
    makeMessage({ ID: "u2", Role: "user", Parts: [{ type: "text", Text: "go" }] }),
    makeMessage({
      ID: "m1",
      Role: "assistant",
      Model: "glm-4.7",
      ReasoningEffort: "high",
      Parts: [
        { type: "thinking", Thinking: "first model thinks" },
        { type: "tool_call", ID: "tc-m1", Name: "view", Input: '{"file_path":"x.ts"}', Finished: true },
        F,
      ],
    }),
    makeMessage({
      ID: "tm1",
      Role: "tool",
      Parts: [{ type: "tool_result", ToolCallID: "tc-m1", Name: "view", Content: "ok", IsError: false }],
    }),
    makeMessage({
      ID: "m2",
      Role: "assistant",
      Model: "kimi-k2",
      ReasoningEffort: "max",
      Parts: [
        { type: "thinking", Thinking: "second model thinks" },
        { type: "tool_call", ID: "tc-m2", Name: "bash", Input: '{"command":"pwd"}', Finished: true },
        F,
      ],
    }),
  ]);

  // First thinking row: "first model thinks" — should have glm-4.7 and high effort
  const row1 = page.locator('[data-test-id="action-row"]').filter({ hasText: "first model thinks" });
  await expect(row1).toBeVisible();
  await expect(row1.getByText("glm-4.7", { exact: true })).toBeVisible();
  const badge1 = row1.locator("span[title='Reasoning effort: high']");
  await expect(badge1).toBeVisible();
  await expect(badge1).toHaveText("[H]");
  // Negative assertions: should NOT have kimi-k2 or max effort
  await expect(row1.getByText("kimi-k2", { exact: true })).toHaveCount(0);
  await expect(row1.locator("span[title='Reasoning effort: max']")).toHaveCount(0);

  // Second thinking row: "second model thinks" — should have kimi-k2 and max effort
  const row2 = page.locator('[data-test-id="action-row"]').filter({ hasText: "second model thinks" });
  await expect(row2).toBeVisible();
  await expect(row2.getByText("kimi-k2", { exact: true })).toBeVisible();
  const badge2 = row2.locator("span[title='Reasoning effort: max']");
  await expect(badge2).toBeVisible();
  await expect(badge2).toHaveText("[XX]");
  // Negative assertions: should NOT have glm-4.7 or high effort
  await expect(row2.getByText("glm-4.7", { exact: true })).toHaveCount(0);
  await expect(row2.locator("span[title='Reasoning effort: high']")).toHaveCount(0);
});
