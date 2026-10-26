import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession, makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
});

async function selectSession(page: import("@playwright/test").Page, id: string, title: string) {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [makeSession({ ID: id, Title: title })] });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await expect(page.getByText(title).first()).toBeVisible();
  await page.getByText(title).first().click();
}

function task(nodeID: string, parentID: string, ordinal: number, childSessionID = "", state = "running") {
  return { nodeID, parentID, kind: "task", taskKind: nodeID, toolCallID: `${nodeID}-call`, state, cause: "", ordinal, depth: ordinal, childSessionID, host: "alive", outcomeCode: "", outcomeSummary: "", outcomeRef: "", noticeSelected: false };
}
function root(rootID: string, ownerSessionID: string, nodes: unknown[]) {
  return { rootID, ownerSessionID, toolCallID: `${rootID}-call`, kind: "batch", taskKind: "", state: "running", cause: "", startedAt: Date.now(), host: "alive", complete: true, counts: { pending: 0, starting: 0, running: nodes.length, cancelling: 0, completed: 0, failed: 0, cancelled: 0, timed_out: 0, interrupted: 0, skipped: 0, active: nodes.length }, delivery: { selected: 0, pending: 0, delivered: 0 }, nodes };
}

 test("renders Batches hierarchy exclusively, sorts children, nests linked roots, expands and reloads from wire", async ({ page }) => {
  await selectSession(page, "batches-ui", "Batches UI");
  const request = await waitForWSSend(page, "get_session_batch_work");
  await sendMockWSMessage(page, { type: "session_batch_work", id: request.id, payload: { v: 1, sessionID: "batches-ui", complete: true, completions: [], roots: [root("nested", "child-session", [task("nested-root", "", 0), task("deep", "nested-root", 1)]), root("parent", "batches-ui", [task("agent", "", 0, "child-session"), task("later", "agent", 9), task("earlier", "agent", 2)])] } });
  await expect(page.getByTestId("live-work-tab-batches")).toHaveText("Batches 2");
  await page.getByTestId("live-work-tab-batches").click();
  await expect(page.getByTestId("batch-tree").getByText("agent")).toBeVisible();
  await expect(page.getByTestId("batch-tree").locator("[data-batch-root=nested]")).toBeVisible();
  await expect(page.getByTestId("batch-tree").locator("[data-batch-node=earlier]").first()).toBeVisible();
  await expect(page.getByTestId("batch-tree").locator("[data-batch-node=deep]")).toBeVisible();
  await page.getByRole("button", { name: "Collapse agent" }).click();
  await expect(page.getByTestId("batch-tree").getByText("earlier")).toHaveCount(0);
  // Restoration is from the wire: re-selecting the session re-sends
  // get_session_batch_work (activation listener), and the same snapshot
  // payload restores the tree; the store-level merge persistence is covered
  // by batchWork.test.ts.
  await selectSession(page, "batches-ui", "Batches UI");
  const restore = await waitForWSSend(page, "get_session_batch_work");
  await sendMockWSMessage(page, { type: "session_batch_work", id: restore.id, payload: { v: 1, sessionID: "batches-ui", complete: true, completions: [], roots: [root("nested", "child-session", [task("nested-root", "", 0), task("deep", "nested-root", 1)]), root("parent", "batches-ui", [task("agent", "", 0, "child-session"), task("later", "agent", 9), task("earlier", "agent", 2)])] } });
  await page.getByTestId("live-work-tab-batches").click();
  await expect(page.getByTestId("batch-tree").getByText("agent")).toBeVisible();
  await expect(page.getByTestId("batch-tree").locator("[data-batch-node=deep]")).toBeVisible();
});

 test("held question and per-node states render from structured wire", async ({ page }) => {
  await selectSession(page, "held-ui", "Held UI");
  const request = await waitForWSSend(page, "get_session_batch_work");
  await sendMockWSMessage(page, { type: "session_batch_work", id: request.id, payload: { v: 1, sessionID: "held-ui", complete: true, roots: [root("held", "held-ui", [{ ...task("question", "", 0), heldQuestion: "Need approval?", noticeSelected: true, noticeState: "pending" }])], completions: [] } });
  await page.getByTestId("live-work-tab-batches").click();
  await expect(page.getByTestId("held-question")).toHaveText("Need approval?");
});
