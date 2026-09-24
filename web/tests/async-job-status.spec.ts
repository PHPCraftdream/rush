import { expect, test } from "@playwright/test";
import { makeMessage, makeSession } from "./helpers/fixtures";
import { sendMockWSMessage, setupMockWS } from "./helpers/mock-ws";

const sessionID = "async-status-session";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
});

test("async command remains running until its completion notice arrives", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "Async Status" })],
  });
  await page.getByText("Async Status").first().click();

  const messages = [
    makeMessage({ ID: "user-async", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "run command" }] }),
    makeMessage({ ID: "assistant-async", SessionID: sessionID, Role: "assistant", Parts: [
      { type: "tool_call", ID: "call-async", Name: "run_command", Input: '{"program":"go","args":["version"]}', Finished: true },
    ] }),
    makeMessage({ ID: "result-async", SessionID: sessionID, Role: "tool", Parts: [
      { type: "tool_result", ToolCallID: "call-async", Name: "run_command", Content: "Async job started", IsError: false, Metadata: '{"async":true,"job_id":"call-async","status":"running"}' },
    ] }),
  ];
  await sendMockWSMessage(page, { type: "messages_list", payload: { SessionID: sessionID, Messages: messages } });
  const row = page.locator('[data-test-id="action-row"]').filter({ hasText: "run_command" });
  const toggle = row.getByTestId("action-row-toggle");
  await expect(toggle.getByText("running…")).toBeVisible();

  await sendMockWSMessage(page, { type: "messages_list", payload: { SessionID: sessionID, Messages: [
    ...messages,
    makeMessage({ ID: "notice-async", SessionID: sessionID, Role: "user", BackgroundJobNotice: false, Parts: [
      { type: "text", Text: "Async job call-async (run_command) finished.\n\ngo version go1.25" },
    ] }),
  ] } });
  await expect(toggle.getByText("done")).toBeVisible();
  await expect(toggle.getByText("running…")).toHaveCount(0);
  await expect(row.getByTestId("tool-result")).toContainText("go version go1.25");
  await expect(page.locator("#msg-notice-async")).toHaveCount(0);
});

test("legacy background shell output attaches to its original command", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "Legacy Background Status" })],
  });
  await page.getByText("Legacy Background Status").first().click();

  const messages = [
    makeMessage({ ID: "user-legacy", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "run npm test" }] }),
    makeMessage({ ID: "assistant-legacy", SessionID: sessionID, Role: "assistant", Parts: [
      { type: "tool_call", ID: "call-legacy", Name: "bash", Input: '{"command":"npm test"}', Finished: true },
    ] }),
    makeMessage({ ID: "result-legacy", SessionID: sessionID, Role: "tool", Parts: [
      { type: "tool_result", ToolCallID: "call-legacy", Name: "bash", Content: "Background shell ID: 00A", IsError: false, Metadata: '{"background":true,"shell_id":"00A"}' },
    ] }),
  ];
  await sendMockWSMessage(page, { type: "messages_list", payload: { SessionID: sessionID, Messages: messages } });
  const row = page.locator('[data-test-id="action-row"]').filter({ hasText: "npm test" });
  await expect(row.getByTestId("action-row-toggle").getByText("running…")).toBeVisible();

  await sendMockWSMessage(page, { type: "messages_list", payload: { SessionID: sessionID, Messages: [
    ...messages,
    makeMessage({ ID: "notice-legacy", SessionID: sessionID, Role: "user", BackgroundJobNotice: true, Parts: [
      { type: "text", Text: "Background job 00A (`npm test`) finished: exit 1, ran 3s.\n\nfailed output" },
    ] }),
  ] } });
  await expect(row.getByTestId("action-row-toggle").getByText("error")).toBeVisible();
  await expect(row.getByTestId("tool-result")).toContainText("failed output");
  await expect(page.locator("#msg-notice-legacy")).toHaveCount(0);
});

test("async sub-agent remains running until its completion notice arrives", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: sessionID, Title: "Async Agent Status" })],
  });
  await page.getByText("Async Agent Status").first().click();

  const messages = [
    makeMessage({ ID: "user-agent", SessionID: sessionID, Role: "user", Parts: [{ type: "text", Text: "delegate" }] }),
    makeMessage({ ID: "assistant-agent", SessionID: sessionID, Role: "assistant", Parts: [
      { type: "tool_call", ID: "call-agent", Name: "agent", Input: '{"prompt":"inspect files"}', Finished: true },
    ] }),
    makeMessage({ ID: "result-agent", SessionID: sessionID, Role: "tool", Parts: [
      { type: "tool_result", ToolCallID: "call-agent", Name: "agent", Content: "Async agent job started", IsError: false, Metadata: '{"async":true,"job_id":"call-agent","status":"running"}' },
    ] }),
  ];
  await sendMockWSMessage(page, { type: "messages_list", payload: { SessionID: sessionID, Messages: messages } });
  const block = page.locator(".sub-agent-block").filter({ hasText: "inspect files" });
  await expect(block.locator(".sub-agent-toggle").getByText("running...")).toBeVisible();

  await sendMockWSMessage(page, { type: "messages_list", payload: { SessionID: sessionID, Messages: [
    ...messages,
    makeMessage({ ID: "notice-agent", SessionID: sessionID, Role: "user", BackgroundJobNotice: true, Parts: [
      { type: "text", Text: "Async job call-agent (agent) failed.\n\nworker failed" },
    ] }),
  ] } });
  await expect(block.locator(".sub-agent-toggle").getByText("error")).toBeVisible();
  await expect(block.locator(".sub-agent-toggle").getByText("running...")).toHaveCount(0);
});
