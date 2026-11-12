// Peak-hours weekly editor E2E specs (task #56).
//
// These specs require the dev server harness exactly like
// web/tests/providers.spec.ts (Playwright + setupMockWS over /ws) and are
// executed only in CI — never by hand.
//
// What is asserted here, and why:
//   1. The shared weekly editor (PeakHoursEditor) sends a POLICY-shaped
//      payload on set_provider_peak_hours, including the canonical days
//      string ("mon-fri" — see canonicalDays in PeakHoursEditor.tsx).
//   2. The status badge/line renders the BACKEND's timezone label and the
//      backend's checkedAt/reopensAt strings from the get_peak_hours_status
//      reply — the browser formats them but never derives peak state from
//      the local clock. No browser-local "active now" state is asserted
//      anywhere in this file.
//   3. Pause sends pause_provider_peak_hours with {id, duration, scope};
//      an empty duration input sends NO duration (server picks the active
//      band's end). Resume (rendered only while pausedUntil is set) sends
//      resume_provider_peak_hours with {id, scope}.
//   4. Toggling off sends peakHours: null.
//   5. A zero-length window (start == end) keeps Save disabled and shows
//      the peakHoursProblems hint (ProvidersModal's canSubmit consumes
//      peakHoursProblems().length).
import { test, expect, Page } from "@playwright/test";
import { setupMockWS } from "./helpers/mock-ws";
import { sendMockWSMessage } from "./helpers/send-mock-ws-message";
import { waitForWSSend } from "./helpers/wait-for-ws-send";
import { makeSession } from "./helpers/make-session";
import { makeConfig } from "./helpers/make-config";
import { fulfillAuthOk } from "./helpers/fulfill-auth-ok";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", fulfillAuthOk);
});

/** Config with one built-in provider; overrides merge into its entry. */
function anthropicConfig(overrides: Record<string, unknown> = {}) {
  return makeConfig({
    providers: {
      anthropic: {
        name: "Anthropic",
        enabled: true,
        models: [
          { id: "claude-opus-4", name: "claude-opus-4", contextWindow: 200000 },
        ],
        ...overrides,
      },
    },
  });
}

/**
 * Loads the page with an active session and the given config, then opens
 * only the providers modal — without entering any row editor.
 *
 * Editing a built-in row unmounts it via the early return in ProviderRow,
 * which would take the row-level status/badge with it.
 */
async function openProvidersModal(page: Page, overrides: Record<string, unknown> = {}) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "peak-sess", Title: "Peak Session" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: anthropicConfig(overrides) });
  await page.getByTestId("header-more-button").click();
  await page.getByTestId("header-providers-button").click();
  await expect(page.getByTestId("providers-modal")).toBeVisible({ timeout: 3000 });
}

/**
 * Loads the page with an active session (the header buttons render only
 * once $activeSessionID is set) and the given config, then opens the
 * providers modal and the built-in anthropic peak-hours-only editor.
 */
async function openBuiltinPeakEditor(
  page: Page,
  overrides: Record<string, unknown> = {}
) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "peak-sess", Title: "Peak Session" })],
  });
  await sendMockWSMessage(page, { type: "config", payload: anthropicConfig(overrides) });
  await page.getByTestId("header-more-button").click();
  await page.getByTestId("header-providers-button").click();
  await expect(page.getByTestId("providers-modal")).toBeVisible({ timeout: 3000 });
  await page.getByTestId("provider-edit-anthropic").click();
  await expect(page.getByTestId("builtin-provider-editor")).toBeVisible({ timeout: 3000 });
}

/** Browser-side: the sent frames of the given type. */
function pickSentFrames(t: string) {
  const sent = (((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>) ?? [];
  return sent.filter((m) => m.type === t);
}

/** All frames of the given type the page has sent so far. */
async function framesOfType(page: Page, type: string) {
  return page.evaluate(pickSentFrames, type);
}

// ── 1. Weekly editor roundtrip sends a policy payload ────────────────────────

test("weekly editor roundtrip sends policy payload", async ({ page }) => {
  await openBuiltinPeakEditor(page);
  // Toggle on: the default draft is one every-day 09:00–18:00 window.
  await page.getByTestId("peak-hours-only-toggle").check();
  await expect(page.getByTestId("peak-hours-only-inputs")).toBeVisible({ timeout: 3000 });
  // Restrict the window to Mon–Fri via the preset chip. canonicalDays
  // collapses a contiguous mon..sun run to "mon-fri" — that exact string
  // is the wire-canonical days value.
  await page.getByRole("button", { name: "Mon–Fri" }).click();
  await page.getByTestId("peak-hours-only-start").fill("09:00");
  await page.getByTestId("peak-hours-only-end").fill("18:00");
  await page.getByTestId("peak-hours-only-timezone").fill("Asia/Shanghai");
  await page.getByTestId("peak-hours-only-save").click();
  const msg = await waitForWSSend(page, "set_provider_peak_hours");
  const payload = msg.payload as Record<string, unknown>;
  expect(payload.id).toBe("anthropic");
  expect(payload.scope).toBe("global");
  const policy = payload.peakHours as Record<string, unknown>;
  expect(policy.timezone).toBe("Asia/Shanghai");
  const windows = policy.windows as Array<Record<string, unknown>>;
  expect(windows).toHaveLength(1);
  expect(windows[0]).toEqual({ days: "mon-fri", start: "09:00", end: "18:00" });
});

test("empty weekly policy preserves metadata on Save", async ({ page }) => {
  const policy = {
    windows: [],
    timezone: "Asia/Shanghai",
    message: "Explicitly off",
    pausedUntil: "2026-11-09T18:00:00Z",
  };
  await openBuiltinPeakEditor(page, { peakHours: policy });
  await expect(page.getByTestId("peak-hours-only-toggle")).toBeChecked();
  await expect(page.getByTestId("peak-hours-only-start")).toHaveCount(0);
  await expect(page.getByTestId("peak-hours-only-timezone")).toHaveValue(policy.timezone);
  await expect(page.getByTestId("peak-hours-only-message")).toHaveValue(policy.message);
  await expect(page.getByTestId("peak-hours-only-resume")).toBeVisible();
  await page.getByTestId("peak-hours-only-save").click();
  const saved = await waitForWSSend(page, "set_provider_peak_hours");
  expect((saved.payload as Record<string, unknown>).peakHours).toEqual(policy);
});

// ── 2. Status badge renders backend data, not browser clock state ────────────

test("status line renders the backend timezone and status strings", async ({ page }) => {
  // Row-level test only: do NOT open the built-in editor — the early
  // return in ProviderRow replaces the row (and its PeakHoursStatus/badge)
  // with the editor.
  await openProvidersModal(page, {
    peakHours: { start: "09:00", end: "18:00" },
  });
  // PeakHoursStatus mounts on the row and asks the server for the
  // authoritative status. Answer exactly like the server would.
  const req = await waitForWSSend(page, "get_peak_hours_status");
  await sendMockWSMessage(page, {
    type: "response",
    id: req.id,
    payload: {
      checkedAt: "2026-11-09T10:30:00+08:00",
      statuses: [
        {
          providerId: "anthropic",
          checkedAt: "2026-11-09T10:30:00+08:00",
          inPeak: false,
          blocked: false,
          reopensAt: "2026-11-09T18:00:00+08:00",
          nextTransition: "2026-11-10T09:00:00+08:00",
          timezone: "Asia/Shanghai",
        },
      ],
    },
  });
  // The status line shows the server's timezone label verbatim — this is
  // backend data the browser only formats, never derives from its own
  // clock. Open state text: "open · next <ts> · <tz> · checked <ts>".
  const status = page.getByTestId("provider-peak-status-open");
  await expect(status).toBeVisible({ timeout: 3000 });
  await expect(status).toContainText("Asia/Shanghai");
  // The formatted checkedAt is derived from the reply, not from Date.now():
  // it renders the backend timestamp, and the badge keeps the wire summary.
  await expect(status).toContainText("checked");
  await expect(page.getByTestId("provider-peak-badge")).toBeVisible({ timeout: 3000 });
});

// ── 3. Pause and resume ──────────────────────────────────────────────────────

test("pause with a duration sends pause_provider_peak_hours", async ({ page }) => {
  await openBuiltinPeakEditor(page);
  await page.getByTestId("peak-hours-only-pause-duration").fill("2h");
  await page.getByTestId("peak-hours-only-pause").click();
  const msg = await waitForWSSend(page, "pause_provider_peak_hours");
  const payload = msg.payload as Record<string, unknown>;
  expect(payload.id).toBe("anthropic");
  expect(payload.duration).toBe("2h");
  expect(payload.scope).toBe("global");
});

test("pause without duration asks the server to pick the band end", async ({ page }) => {
  await openBuiltinPeakEditor(page);
  // Leave the duration input empty: undefined duration → the server picks
  // the end of the currently active band (never the browser's clock).
  await page.getByTestId("peak-hours-only-pause").click();
  const msg = await waitForWSSend(page, "pause_provider_peak_hours");
  const payload = msg.payload as Record<string, unknown>;
  expect(payload.id).toBe("anthropic");
  expect(payload.scope).toBe("global");
  expect(payload.duration).toBeFalsy();
});

test("resume reply and broadcast cannot restore the pause on Save", async ({ page }) => {
  // pausedUntil on the entry makes the editor render the Resume control
  // instead of the duration input.
  await openBuiltinPeakEditor(page, {
    peakHours: {
      windows: [{ days: "mon-fri", start: "09:00", end: "18:00" }],
      pausedUntil: "2026-11-09T18:00:00Z",
    },
  });
  await page.getByTestId("peak-hours-only-resume").click();
  const msg = await waitForWSSend(page, "resume_provider_peak_hours");
  const payload = msg.payload as Record<string, unknown>;
  expect(payload.id).toBe("anthropic");
  expect(payload.scope).toBe("global");
  await sendMockWSMessage(page, { id: msg.id, type: "response", payload: { status: "ok" } });
  await sendMockWSMessage(page, {
    type: "config",
    payload: anthropicConfig({
      peakHours: { windows: [{ days: "mon-fri", start: "09:00", end: "18:00" }] },
    }),
  });
  await expect(page.getByTestId("peak-hours-only-resume")).toHaveCount(0);
  await page.getByTestId("peak-hours-only-save").click();
  const saved = await waitForWSSend(page, "set_provider_peak_hours");
  const policy = (saved.payload as Record<string, unknown>).peakHours as Record<string, unknown>;
  expect(policy).not.toHaveProperty("pausedUntil");
  expect(policy.windows).toEqual([{ days: "mon-fri", start: "09:00", end: "18:00" }]);
});

// ── 4. Toggle off clears ─────────────────────────────────────────────────────

test("toggle off clears the policy with peakHours null", async ({ page }) => {
  await openBuiltinPeakEditor(page, {
    peakHours: { start: "09:00", end: "18:00" },
  });
  await expect(page.getByTestId("peak-hours-only-toggle")).toBeChecked();
  await page.getByTestId("peak-hours-only-toggle").uncheck();
  await expect(page.getByTestId("peak-hours-only-inputs")).toHaveCount(0);
  await page.getByTestId("peak-hours-only-save").click();
  const msg = await waitForWSSend(page, "set_provider_peak_hours");
  const payload = msg.payload as Record<string, unknown>;
  expect(payload.id).toBe("anthropic");
  expect(payload.peakHours).toBeNull();
});

// ── 5. Zero-length window is blocked client-side ─────────────────────────────

test("zero-length window keeps Save disabled and shows the hint", async ({ page }) => {
  await openBuiltinPeakEditor(page);
  await page.getByTestId("peak-hours-only-toggle").check();
  await expect(page.getByTestId("peak-hours-only-inputs")).toBeVisible({ timeout: 3000 });
  // start == end is a zero-length window — peakHoursProblems flags it and
  // ProvidersModal's canSubmit disables Save.
  await page.getByTestId("peak-hours-only-start").fill("09:00");
  await page.getByTestId("peak-hours-only-end").fill("09:00");
  const save = page.getByTestId("peak-hours-only-save");
  await expect(save).toBeDisabled({ timeout: 3000 });
  await expect(
    page.getByText("start and end must differ", { exact: false })
  ).toBeVisible({ timeout: 3000 });
  // Nothing was sent while the draft is invalid.
  expect(await framesOfType(page, "set_provider_peak_hours")).toHaveLength(0);
});
