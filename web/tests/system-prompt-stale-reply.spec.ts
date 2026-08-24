/**
 * Stale get_system_prompt reply guard — regression specs for review F5
 * (2026-08-24 readonly review; task #693).
 *
 * Pre-fix behavior under test: SystemPromptModal fetched the system
 * prompt by subscribing to ALL `system_prompt` events and sending
 * `get_system_prompt` with no correlation id. When the active session
 * flipped while the modal was open (the full-screen overlay blocks
 * sidebar clicks, but WS-driven switches — e.g. a broadcast
 * session_created from another client, see useWS.ts — pass straight
 * through), the modal re-fetched for session B and B's fresh unfiltered
 * listener swallowed A's still-in-flight reply. The textarea then showed
 * A's prompt while the modal was contextually "for" B, and Save wrote
 * A's prompt over B's.
 *
 * Expected (fixed) behavior, mirroring the save-path discipline task
 * #683 (beed48a4) established for this same modal: the fetch carries a
 * msgID; only a reply matching that id AND the payload's echoed
 * sessionID may populate the editor; error replies, timeouts and
 * disconnects surface explicitly instead of hanging on "Loading…".
 *
 * Server wire contract (internal/server/handlers_sessions.go,
 * handleGetSystemPrompt): success replies `system_prompt` with the
 * request's id and payload {sessionID, content}; failure replies `error`
 * with the request's id.
 */

import { test, expect, Page } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeConfig, makeSession } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
});

const SESSION_A = { ID: "sess-a", Title: "Session Alpha" };
const SESSION_B = { ID: "sess-b", Title: "Session Beta" };

/** Boots the app with one session (A) active and opens the System Prompt
 * modal for it; returns once the modal is visible (its get_system_prompt
 * for A is about to hit the wire). */
async function openModalForSessionA(page: Page) {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession(SESSION_A)],
  });
  const entry = page.getByText("Session Alpha").first();
  await expect(entry).toBeVisible({ timeout: 3000 });
  await entry.click();
  await page.getByTestId("header-more-button").click();
  await expect(page.getByTestId("header-logs-button")).toBeVisible({ timeout: 2000 });
  await page.getByTestId("header-prompt-button").click();
  await expect(page.getByText("System Prompt")).toBeVisible({ timeout: 3000 });
}

// ── F5: stale cross-session reply ─────────────────────────────────────────────

test("BUG regression: session A's late get_system_prompt reply is discarded after the active session switched to B", async ({ page }) => {
  await openModalForSessionA(page);

  const cmdA = await waitForWSSend(page, "get_system_prompt");
  expect((cmdA.payload as { sessionID: string }).sessionID).toBe("sess-a");
  // The fetch must be correlated: an id is required for the reply filter.
  expect(cmdA.id).toBeTruthy();

  // Modal still loading (A's reply deliberately withheld). Flip the active
  // session underneath it the way a broadcast session_created from another
  // client does — the modal overlay swallows sidebar clicks, but this path
  // re-renders the modal with a new sessionID prop.
  await sendMockWSMessage(page, {
    type: "session_created",
    payload: makeSession(SESSION_B),
  });

  // The modal must re-fetch for B with a NEW correlation id…
  // (waitForWSSend returns the LAST — i.e. most recent — matching frame)
  const cmdB = await waitForWSSend(page, "get_system_prompt");
  expect((cmdB.payload as { sessionID: string }).sessionID).toBe("sess-b");
  expect(cmdB.id).toBeTruthy();
  expect(cmdB.id).not.toBe(cmdA.id);

  // …and now A's stale reply finally lands.
  await sendMockWSMessage(page, {
    type: "system_prompt",
    id: cmdA.id,
    payload: { sessionID: "sess-a", content: "PROMPT_BELONGS_TO_A" },
  });

  // A's content must NOT populate the modal — still loading, no textarea.
  await expect(page.locator(".fixed textarea")).toHaveCount(0);
  await expect(page.getByText("Loading")).toBeVisible();

  // B's genuine reply arrives and populates the editor.
  await sendMockWSMessage(page, {
    type: "system_prompt",
    id: cmdB.id,
    payload: { sessionID: "sess-b", content: "PROMPT_BELONGS_TO_B" },
  });
  const textarea = page.locator(".fixed textarea");
  await expect(textarea).toBeVisible({ timeout: 3000 });
  await expect(textarea).toHaveValue("PROMPT_BELONGS_TO_B");

  // End-to-end invariant: whatever gets saved from this modal targets B.
  await textarea.fill("EDITED_FOR_B");
  await page.locator(".fixed button", { hasText: "Save" }).click();
  const save = await waitForWSSend(page, "set_system_prompt");
  const savePayload = save.payload as { sessionID: string; content: string };
  expect(savePayload.sessionID).toBe("sess-b");
  expect(savePayload.content).toBe("EDITED_FOR_B");
});

// ── Correlation strictness ────────────────────────────────────────────────────

test("system_prompt events with no correlation id never populate the modal", async ({ page }) => {
  await openModalForSessionA(page);
  const cmd = await waitForWSSend(page, "get_system_prompt");

  // A stray/un-correlated system_prompt event (no id) must be ignored.
  await sendMockWSMessage(page, {
    type: "system_prompt",
    payload: { sessionID: "sess-a", content: "ROGUE_UNCORRELATED" },
  });
  await expect(page.locator(".fixed textarea")).toHaveCount(0);
  await expect(page.getByText("Loading")).toBeVisible();

  // The correlated reply still works.
  await sendMockWSMessage(page, {
    type: "system_prompt",
    id: cmd.id,
    payload: { sessionID: "sess-a", content: "GENUINE_PROMPT" },
  });
  const textarea = page.locator(".fixed textarea");
  await expect(textarea).toBeVisible({ timeout: 3000 });
  await expect(textarea).toHaveValue("GENUINE_PROMPT");
});

test("a correlated reply whose payload sessionID mismatches the modal's session is not applied", async ({ page }) => {
  await openModalForSessionA(page);
  const cmd = await waitForWSSend(page, "get_system_prompt");

  // Right id, WRONG session in the payload (defense in depth on top of the
  // id filter — the server echoes sessionID in the payload precisely so
  // clients can verify it).
  await sendMockWSMessage(page, {
    type: "system_prompt",
    id: cmd.id,
    payload: { sessionID: "sess-other", content: "PROMPT_FOR_SOMEONE_ELSE" },
  });
  await expect(page.locator(".fixed textarea")).toHaveCount(0);
  await expect(page.getByText("Loading")).toBeVisible();

  await sendMockWSMessage(page, {
    type: "system_prompt",
    id: cmd.id,
    payload: { sessionID: "sess-a", content: "GENUINE_PROMPT" },
  });
  const textarea = page.locator(".fixed textarea");
  await expect(textarea).toBeVisible({ timeout: 3000 });
  await expect(textarea).toHaveValue("GENUINE_PROMPT");
});

// ── Explicit failure handling (no infinite "Loading…") ────────────────────────

test("an error reply to get_system_prompt surfaces in the modal instead of hanging on Loading", async ({ page }) => {
  await openModalForSessionA(page);
  const cmd = await waitForWSSend(page, "get_system_prompt");

  // Exact wire shape from handleGetSystemPrompt's failure path.
  await sendMockWSMessage(page, { type: "error", id: cmd.id, error: "session not found" });

  await expect(page.getByText("Loading…", { exact: true })).not.toBeVisible({ timeout: 3000 });
  await expect(page.locator(".fixed", { hasText: "session not found" })).toBeVisible();
  const textarea = page.locator(".fixed textarea");
  await expect(textarea).toBeVisible();
  await expect(textarea).toHaveValue("");
  await expect(page.locator(".fixed button", { hasText: "Save" })).toBeDisabled();
});

test("a get_system_prompt that never gets a reply times out with a visible error", async ({ page }) => {
  await openModalForSessionA(page);
  await waitForWSSend(page, "get_system_prompt");

  // No reply is ever delivered.
  await expect(page.getByText("Timed out waiting for the system prompt")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText("Loading…", { exact: true })).not.toBeVisible();
  await expect(page.locator(".fixed button", { hasText: "Save" })).toBeDisabled();
});

test("losing the connection while the fetch is in flight fails it explicitly", async ({ page }) => {
  await openModalForSessionA(page);
  await waitForWSSend(page, "get_system_prompt");

  await page.evaluate(() => {
    const sock = (window as unknown as Record<string, unknown>)["__mockWS"] as { close: () => void } | null;
    sock?.close();
  });

  await expect(page.getByText("Connection lost while loading the system prompt")).toBeVisible({ timeout: 3000 });
  await expect(page.getByText("Loading…", { exact: true })).not.toBeVisible();
});
