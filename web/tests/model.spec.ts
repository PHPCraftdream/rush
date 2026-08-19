import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

// ── Model display ──────────────────────────────────────────────────────────

// GENUINELY BROKEN — left red on purpose, not stale and not a selector
// problem. ModelSelector.tsx still contains its own `if (!session ||
// allModels.length === 0)` fallback (line ~260) that renders a static
// <span> badge with the model's displayName instead of the clickable
// dropdown button. That code is dead: ModelSelector.tsx is mounted from
// exactly one place, ChatToolbar.tsx:422-423, and ChatToolbar itself has
// `if (!activeSessionID) return null;` (line ~195) — so ModelSelector never
// gets a chance to run with session=null in the current app. Before commit
// 89a07919 folded Header into ChatToolbar, the old standalone Header.tsx
// rendered unconditionally and this fallback was reachable. This is the same
// root cause as the toolbar-visibility regression documented at length in
// ui.spec.ts, but it manifests differently here: not just "control
// unreachable" but "an entire source-level conditional branch is now
// provably unreachable in the running app". Real defect, out of this test
// file's scope to fix (see task #567 report for the writeup); left failing
// rather than deleted or weakened.
test("header shows model name from config without session", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig(),
  });
  // When no session selected, shows static badge with "smart" model
  await expect(page.getByText("claude-opus-4")).toBeVisible({ timeout: 2000 });
});

test("header shows model selector button when session is active", async ({
  page,
}) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "m-sel-1", Title: "Model Session" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig(),
  });
  await page.getByText("Model Session").first().click();
  // The model name appears in a clickable button with dropdown arrow
  await expect(
    page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" })
  ).toBeVisible({ timeout: 2000 });
});

// ── Dropdown open/close ────────────────────────────────────────────────────

test("clicking model button opens dropdown", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "dd-1", Title: "DD Session" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("DD Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("DD Session").first().click();
  await expect(page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" })).toBeVisible({ timeout: 3000 });

  await page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" }).click();
  await expect(page.getByPlaceholder("Search models…")).toBeVisible({ timeout: 2000 });
});

test("model dropdown shows all available models", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "all-1", Title: "All Models" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("All Models").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("All Models").first().click();
  await expect(page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" })).toBeVisible({ timeout: 3000 });

  await page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" }).click();
  // Both models should appear in dropdown
  await expect(page.getByText("claude-haiku-4").first()).toBeVisible({ timeout: 2000 });
  // Provider info shown below each model name
  await expect(page.getByTestId("model-dropdown").getByText("anthropic").first()).toBeVisible();
});

test("model dropdown closes on outside click", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "cls-1", Title: "Close Test" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Close Test").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Close Test").first().click();
  await expect(page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" })).toBeVisible({ timeout: 3000 });

  await page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" }).click();
  await expect(page.getByPlaceholder("Search models…")).toBeVisible({ timeout: 2000 });

  // Click outside — the sidebar header is well outside the dropdown's
  // click-outside boundary (ModelSelector's own `ref`, scoped to the
  // toolbar). There is no <header>/<h1> element in this app any more (the
  // old standalone Header.tsx was folded into ChatToolbar — see ui.spec.ts).
  await page.getByTestId("sidebar-header").click();
  await expect(page.getByPlaceholder("Search models…")).not.toBeVisible({ timeout: 2000 });
});

// ── Search ─────────────────────────────────────────────────────────────────

test("search filters models by name", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "srch-1", Title: "Search Session" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: {
          enabled: true,
          models: [
            { id: "claude-opus-4", name: "claude-opus-4" },
            { id: "claude-haiku-4", name: "claude-haiku-4" },
          ],
        },
        openai: { models: [{ id: "gpt-4o", name: "gpt-4o" }] },
      },
    }),
  });
  await expect(page.getByText("Search Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Search Session").first().click();
  await expect(page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" })).toBeVisible({ timeout: 3000 });
  await page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" }).click();

  await page.getByPlaceholder("Search models…").fill("haiku");
  await expect(page.getByText("claude-haiku-4").first()).toBeVisible({ timeout: 2000 });
  // gpt-4o doesn't match "haiku"
  await expect(page.getByTestId("model-dropdown").getByText("gpt-4o")).not.toBeVisible();
});

test("search shows no results message when nothing matches", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "nores-1", Title: "No Results" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("No Results").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("No Results").first().click();
  await expect(page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" })).toBeVisible({ timeout: 3000 });
  await page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" }).click();

  await page.getByPlaceholder("Search models…").fill("xyznonexistent");
  await expect(page.getByText("No models found")).toBeVisible({ timeout: 2000 });
});

// ── Model selection ────────────────────────────────────────────────────────

test("selecting a model from dropdown updates header display", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "pick-1", Title: "Pick Session" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Pick Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Pick Session").first().click();
  await expect(page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" })).toBeVisible({ timeout: 3000 });

  // Open dropdown and select "fast" model
  await page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" }).click();
  // Click the claude-haiku-4 option in the dropdown
  await page.getByTestId("model-dropdown").getByText("claude-haiku-4").click();

  // Header large model button now shows the selected model
  await expect(
    page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-haiku-4" }).first()
  ).toBeVisible({ timeout: 2000 });
});

// ── Per-session independence ───────────────────────────────────────────────

test("each session has independent model selection", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [
      makeSession({ ID: "ind-1", Title: "Session One" }),
      makeSession({ ID: "ind-2", Title: "Session Two" }),
    ],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });

  // Select Session One and pick "fast" model
  await expect(page.getByText("Session One")).toBeVisible({ timeout: 3000 });
  await page.getByText("Session One").click();
  await expect(page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" })).toBeVisible({ timeout: 3000 });
  await page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" }).click();
  await page.getByTestId("model-dropdown").getByText("claude-haiku-4").click();
  await expect(
    page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-haiku-4" }).first()
  ).toBeVisible({ timeout: 2000 });

  // Switch to Session Two — should show default "smart" model
  await page.getByText("Session Two").click();
  await expect(
    page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" })
  ).toBeVisible({ timeout: 2000 });

  // Switch back to Session One — should still show "fast"
  await page.getByText("Session One").click();
  await expect(
    page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-haiku-4" }).first()
  ).toBeVisible({ timeout: 2000 });
});

// ── Settings model panel (removed feature) ─────────────────────────────────
//
// DELETED, not re-pointed. Both tests targeted a "Session Models" section
// (heading text "Session Models", labels "Large model"/"Fast model", plain
// <select> elements) that lived inside the general Settings modal. That
// section was removed in commit f1a406bb ("session management with
// hash-based routing and inline rename") — `git log -S"Session Models"`
// shows it deleted there and never reintroduced. Per-session model
// selection today happens exclusively through the ModelSelector dropdown
// buttons in the toolbar (button[title='Smart (strong) model'] /
// button[title='Fast (cheap) model']), which the rest of this file already
// covers (see "selecting a model from dropdown updates header display" and
// the small-model-selector tests below). SettingsModal.tsx today has no
// session-model section at all, and ScopedModelsModal.tsx (opened by the
// "Default models" toolbar button) is a different feature — it edits
// global/workspace scope defaults, not this session's override, and uses
// different labels ("Smart (strong)"/"Fast (cheap)", not "Smart
// model"/"Fast model") — so re-pointing these two tests at it would silently
// test something else while keeping the original test names/intent.
// getByTitle("Settings") was also a strict-mode violation independent of the
// above (Playwright's getByTitle string match is case-insensitive substring
// by default, so it also matched the "MCP server settings" button) — moot
// once the tests targeting a "Settings"-only click are gone.

// ── Fast model selector ────────────────────────────────────────────────────

test("header shows small model selector button when session is active", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sm-1", Title: "Fast Model Session" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Fast Model Session").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Fast Model Session").first().click();
  // Fast model button with ⚡ icon
  await expect(
    page.locator("button[title='Fast (cheap) model']")
  ).toBeVisible({ timeout: 2000 });
});

test("clicking small model button opens its own dropdown", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sm-2", Title: "Fast DD" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText("Fast DD").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Fast DD").first().click();
  await page.locator("button[title='Fast (cheap) model']").click();
  await expect(page.getByPlaceholder("Search models…")).toBeVisible({ timeout: 2000 });
});

test("selecting from small model dropdown updates small selector", async ({ page }) => {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "sm-3", Title: "Fast Pick" })],
  });
  await sendMockWSMessage(page, {
    type: "config",
    payload: makeConfig({
      providers: {
        anthropic: {
          enabled: true,
          models: [
            { id: "claude-opus-4", name: "claude-opus-4" },
            { id: "claude-haiku-4", name: "claude-haiku-4" },
          ],
        },
        // enabled: true is required — buildProviderGroups() (ModelSelector.tsx)
        // excludes any non-CLI provider without it since commit 30dcb96a ("hide
        // providers without an API key from model selection"), so gpt-4o-mini
        // was silently never offered in the dropdown this test opens below.
        openai: { enabled: true, models: [{ id: "gpt-4o-mini", name: "gpt-4o-mini" }] },
      },
    }),
  });
  await expect(page.getByText("Fast Pick").first()).toBeVisible({ timeout: 3000 });
  await page.getByText("Fast Pick").first().click();
  // Open small model dropdown
  await page.locator("button[title='Fast (cheap) model']").click();
  // Pick gpt-4o-mini
  await page.getByTestId("model-dropdown").getByText("gpt-4o-mini").click();
  // Fast model button now shows gpt-4o-mini
  await expect(
    page.locator("button[title='Fast (cheap) model']").filter({ hasText: "gpt-4o-mini" })
  ).toBeVisible({ timeout: 2000 });
  // Smart model button unchanged
  await expect(
    page.locator("button[title='Smart (strong) model']").filter({ hasText: "claude-opus-4" })
  ).toBeVisible({ timeout: 2000 });
});
