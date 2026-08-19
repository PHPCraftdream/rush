/**
 * Model name display below assistant messages.
 *
 * Covers:
 *  - Model name appears on assistant messages that have a Model field
 *  - Model name is hidden by default, visible on hover
 *  - User messages do NOT show model name
 *  - Messages without Model field show no model label
 *  - Multiple messages each show their own model name
 *  - model_updated event keeps the model name in sync
 */

import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// ── Basic rendering ───────────────────────────────────────────────────────────

test("assistant message with Model shows model name on hover", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "mm-1", Title: "Model Label" })],
  });
  await expect(page.getByText("Model Label").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Model Label").first().click();
  // useWS.ts's messages_list handler drops the payload unless its
  // SessionID matches the active session; makeMessage()'s "sess-1"
  // default never matches "mm-1", so it must be pinned here.
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessage({
        ID: "mm-m1",
        SessionID: "mm-1",
        Role: "assistant",
        Parts: [{ type: "text", Text: "Here is my answer." }],
        Model: "claude-opus-4",
      }),
    ],
  });
  await expect(page.getByText("Here is my answer.")).toBeVisible({ timeout: 2000 });
  // AssistantHoverActions.tsx (where the model label lives) is only
  // mounted while Message.tsx's `hovered` state is true — the test title
  // says "on hover" but the original body never hovered the row.
  const msgRow = page.getByText("Here is my answer.").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await msgRow.hover();
  await expect(page.getByText("claude-opus-4")).toBeVisible({ timeout: 2000 });
});

test("user message does NOT show a model name", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "mm-2", Title: "User No Model" })],
  });
  await expect(page.getByText("User No Model").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("User No Model").first().click();
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessage({
        ID: "mm-u1",
        SessionID: "mm-2",
        Role: "user",
        Parts: [{ type: "text", Text: "User question here" }],
        Model: "some-model",
      }),
    ],
  });
  await expect(page.getByText("User question here")).toBeVisible({ timeout: 2000 });
  // Model name must NOT appear in the user bubble area. UserContent.tsx
  // renders the user bubble as .msg-bubble-user, not .bg-accent (which no
  // longer exists in source — the old selector matched zero elements, so
  // this assertion was passing vacuously rather than actually checking
  // anything).
  const userBubble = page.locator(".msg-bubble-user").filter({ hasText: "User question here" });
  await expect(userBubble).toBeVisible({ timeout: 1000 });
  await expect(userBubble.getByText("some-model")).not.toBeVisible({ timeout: 1000 });
});

test("assistant message without Model field shows no model label", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "mm-3", Title: "No Model Field" })],
  });
  await expect(page.getByText("No Model Field").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("No Model Field").first().click();
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessage({
        ID: "mm-a1",
        SessionID: "mm-3",
        Role: "assistant",
        Parts: [{ type: "text", Text: "Response without model" }],
        Model: "",
      }),
    ],
  });
  await expect(page.getByText("Response without model")).toBeVisible({ timeout: 2000 });
  // No span with font-mono text in the assistant row (where model label lives)
  const msgRow = page.getByText("Response without model").locator("../..");
  await expect(msgRow.locator("span.font-mono")).not.toBeVisible({ timeout: 1000 });
});

// ── Multiple messages with different models ───────────────────────────────────

test("each assistant message shows its own model name", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "mm-4", Title: "Multi Model" })],
  });
  await expect(page.getByText("Multi Model").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Multi Model").first().click();
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessage({
        ID: "mm-a2",
        SessionID: "mm-4",
        Role: "assistant",
        Parts: [{ type: "text", Text: "First response" }],
        Model: "claude-opus-4",
      }),
      makeMessage({
        ID: "mm-a3",
        SessionID: "mm-4",
        Role: "assistant",
        Parts: [{ type: "text", Text: "Second response" }],
        Model: "gpt-4o",
      }),
    ],
  });
  await expect(page.getByText("First response")).toBeVisible({ timeout: 2000 });
  await expect(page.getByText("Second response")).toBeVisible({ timeout: 2000 });
  // Model label is hover-gated per Message.tsx instance (AssistantHoverActions
  // only mounts while that row's own `hovered` state is true), so each row
  // must be hovered individually to reveal its own label.
  const row1 = page.getByText("First response").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row1.hover();
  await expect(row1.getByText("claude-opus-4")).toBeVisible({ timeout: 2000 });

  const row2 = page.getByText("Second response").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row2.hover();
  await expect(row2.getByText("gpt-4o")).toBeVisible({ timeout: 2000 });
});

// ── Streaming: model_updated event ───────────────────────────────────────────

test("model name updates when message_updated brings a new Model value", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "mm-5", Title: "Stream Model" })],
  });
  await expect(page.getByText("Stream Model").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Stream Model").first().click();

  // Partial message with no Model yet
  await sendMockWSMessage(page, {
    type: "message_created",
    payload: makeMessage({
      ID: "mm-s1",
      SessionID: "mm-5",
      Role: "assistant",
      Parts: [{ type: "text", Text: "Streaming..." }],
      Model: "",
    }),
  });
  await expect(page.getByText("Streaming...")).toBeVisible({ timeout: 2000 });

  // Full message arrives with Model
  await sendMockWSMessage(page, {
    type: "message_updated",
    payload: makeMessage({
      ID: "mm-s1",
      SessionID: "mm-5",
      Role: "assistant",
      Parts: [{ type: "text", Text: "Done streaming" }],
      Model: "claude-sonnet-4-6",
    }),
  });
  await expect(page.getByText("Done streaming")).toBeVisible({ timeout: 2000 });
  // Model label only mounts while the row is hovered.
  const row = page.getByText("Done streaming").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row.hover();
  await expect(row.getByText("claude-sonnet-4-6")).toBeVisible({ timeout: 2000 });
});

// ── Copy button co-exists with model label ────────────────────────────────────

test("both copy button and model name are visible on hover", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "mm-6", Title: "Copy+Model" })],
  });
  await expect(page.getByText("Copy+Model").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Copy+Model").first().click();
  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: [
      makeMessage({
        ID: "mm-c1",
        SessionID: "mm-6",
        Role: "assistant",
        Parts: [{ type: "text", Text: "Copyable assistant message" }],
        Model: "claude-haiku-4",
      }),
    ],
  });
  await expect(page.getByText("Copyable assistant message")).toBeVisible({ timeout: 2000 });
  // Both the Copy button and the model label live in AssistantHoverActions,
  // which only mounts while the row is hovered (not "always visible").
  const row = page.getByText("Copyable assistant message").locator("xpath=ancestor::div[contains(@class,'msg-row')]");
  await row.hover();
  await expect(row.getByRole("button", { name: "Copy", exact: true })).toBeVisible({ timeout: 2000 });
  await expect(row.getByText("claude-haiku-4")).toBeVisible({ timeout: 2000 });
});
