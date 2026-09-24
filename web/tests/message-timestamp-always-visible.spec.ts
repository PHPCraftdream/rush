/**
 * Regression coverage for commit 10e32c56: the message timestamp badge is
 * always visible. Message.tsx renders <TimeBadge epochSec={message.CreatedAt}/>
 * unconditionally in the fixed msg-actions strip for BOTH user and assistant
 * messages — it is no longer gated on `hovered` (AssistantHoverActions dropped
 * its own hover-only copy in the same commit).
 *
 * TimeBadge (web/src/components/Message/TimeBadge.tsx) renders the epoch-
 * seconds timestamp as local "HH:MM:SS" text with a full
 * "YYYY-MM-DD HH:MM:SS UTC±HH:MM" title tooltip.
 */

import { test, expect, Page } from "@playwright/test";
import { setupMockWS, sendMockWSMessage } from "./helpers/mock-ws";
import { makeSession, makeMessage } from "./helpers/fixtures";

// Epoch SECONDS — TimeBadge multiplies by 1000. makeMessage's ms-scale
// default would render a nonsense time, so pass real epoch-second values.
const USER_TS = 1700000000;
const ASSISTANT_TS = 1700000065;

// Same local-time formatting as TimeBadge. The test process and the browser
// share the machine's timezone, so the expected strings can be computed here.
function expectedBadgeText(epochSec: number): string {
  const d = new Date(epochSec * 1000);
  return [d.getHours(), d.getMinutes(), d.getSeconds()]
    .map((n) => String(n).padStart(2, "0"))
    .join(":");
}

function expectedBadgeTitle(epochSec: number): string {
  const d = new Date(epochSec * 1000);
  const pad = (n: number) => String(n).padStart(2, "0");
  const tz = -d.getTimezoneOffset();
  const tzAbs = Math.abs(tz);
  const tzSign = tz >= 0 ? "+" : "-";
  return (
    `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}` +
    ` ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}` +
    ` UTC${tzSign}${pad(Math.floor(tzAbs / 60))}:${pad(tzAbs % 60)}`
  );
}

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

async function openSessionWithMessages(page: Page) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: "ts-sess", Title: "Timestamp Session" })],
  });
  await page.getByText("Timestamp Session").first().click();
  await expect(page.getByTestId("chat-input-textarea")).toBeEnabled({ timeout: 5000 });

  await sendMockWSMessage(page, {
    type: "messages_list",
    payload: {
      SessionID: "ts-sess",
      Messages: [
        makeMessage({
          ID: "ts-user",
          SessionID: "ts-sess",
          Role: "user",
          Parts: [{ type: "text", Text: "hello" }],
          CreatedAt: USER_TS,
        }),
        makeMessage({
          ID: "ts-assistant",
          SessionID: "ts-sess",
          Role: "assistant",
          Parts: [
            { type: "text", Text: "world" },
            { type: "finish", Reason: "end_turn", Message: "", Details: "" },
          ],
          CreatedAt: ASSISTANT_TS,
        }),
      ],
    },
  });

  await expect(page.getByText("hello")).toBeVisible({ timeout: 5000 });
  await expect(page.getByText("world")).toBeVisible();
}

test("user message shows its timestamp badge without hovering", async ({ page }) => {
  await openSessionWithMessages(page);

  // No pointer ever moves over the row, so the interactive hover controls
  // stay unmounted — yet the badge must still be there (the 10e32c56 change).
  const actions = page.locator("#msg-ts-user .msg-actions");
  await expect(actions.locator("button")).toHaveCount(0);
  const badge = actions.locator("span.tabular-nums");
  await expect(badge).toHaveCount(1);
  await expect(badge).toHaveText(expectedBadgeText(USER_TS));
  await expect(badge).toHaveAttribute("title", expectedBadgeTitle(USER_TS));
});

test("assistant message shows its timestamp badge without hovering", async ({ page }) => {
  await openSessionWithMessages(page);

  const actions = page.locator("#msg-ts-assistant .msg-actions");
  await expect(actions.locator("button")).toHaveCount(0);
  const badge = actions.locator("span.tabular-nums");
  await expect(badge).toHaveCount(1);
  await expect(badge).toHaveText(expectedBadgeText(ASSISTANT_TS));
  await expect(badge).toHaveAttribute("title", expectedBadgeTitle(ASSISTANT_TS));
});

async function expectActionSpacing(page: Page, id: string) {
  const row = page.locator(`#msg-${id}`);
  await row.hover();
  const actions = row.locator(".msg-actions");
  await expect(actions.locator("button").first()).toBeVisible();
  const timestamp = await actions.locator(":scope > span.tabular-nums").boundingBox();
  const firstButton = await actions.locator("button").first().boundingBox();
  expect(timestamp).not.toBeNull();
  expect(firstButton).not.toBeNull();
  expect(firstButton!.x - (timestamp!.x + timestamp!.width)).toBeGreaterThanOrEqual(15);
  const centerDelta = timestamp!.y + timestamp!.height / 2 - firstButton!.y - firstButton!.height / 2;
  expect(centerDelta).toBeGreaterThanOrEqual(0);
  expect(centerDelta).toBeLessThanOrEqual(3);
}

test("message actions leave space after the timestamp on hover", async ({ page }) => {
  await openSessionWithMessages(page);
  await expectActionSpacing(page, "ts-user");
  await expectActionSpacing(page, "ts-assistant");
});
