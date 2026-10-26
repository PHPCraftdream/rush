import { test, expect } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession, makeConfig } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
});
async function select(page: import("@playwright/test").Page) {
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [makeSession({ ID: "control-ui", Title: "Control UI" })] });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig() });
  await page.getByText("Control UI").first().click();
}
function treePayload() {
  const node = { nodeID: "parent-task", parentID: "", kind: "task", taskKind: "parent task", toolCallID: "t", state: "running", cause: "", ordinal: 0, depth: 0, childSessionID: "child", host: "alive", outcomeCode: "", outcomeSummary: "", outcomeRef: "", noticeSelected: false };
  const child = { ...node, nodeID: "leaf", taskKind: "leaf task", childSessionID: "", heldQuestion: "Approve?" };
  const addNode = { ...node, nodeID: "add-node", taskKind: "", kind: "batch", childSessionID: "" };
  const mkRoot = (rootID: string, ownerSessionID: string, nodes: object[]) => ({ rootID, ownerSessionID, toolCallID: "", kind: "batch", taskKind: "", state: "running", cause: "", startedAt: 1, host: "alive", complete: true, counts: { pending: 0, starting: 0, running: 1, cancelling: 0, completed: 0, failed: 0, cancelled: 0, timed_out: 0, interrupted: 0, skipped: 0, active: 1 }, delivery: { selected: 0, pending: 0, delivered: 0 }, nodes });
  return { v: 1, sessionID: "control-ui", complete: true, completions: [], roots: [mkRoot("root", "control-ui", [node]), mkRoot("child-root", "child", [child, addNode])] };
}
async function enterTree(page: import("@playwright/test").Page) {
  await select(page);
  const request = await waitForWSSend(page, "get_session_batch_work");
  await sendMockWSMessage(page, { type: "session_batch_work", id: request.id, payload: treePayload() });
  await page.getByTestId("live-work-tab-batches").click();
}

test("nested node controls carry its owning session and root; stop acceptance remains pending", async ({ page }) => {
  await enterTree(page);
  const node = page.locator('[data-batch-node="leaf"]');
  await node.getByRole("button", { name: "Stop leaf" }).click();
  const sent = await waitForWSSend(page, "batch_stop");
  expect(sent.payload).toMatchObject({ sessionID: "child", rootID: "child-root", nodeID: "leaf" });
  await sendMockWSMessage(page, { type: "batch_control_result", id: sent.id, payload: { sessionID: "child", rootID: "child-root", nodeID: "leaf", action: "batch_stop", result: "accepted", ok: true } });
  const statusReq = await waitForWSSend(page, "batch_status");
  await sendMockWSMessage(page, { type: "session_batch_work", id: statusReq.id, payload: treePayload() });
  await expect(node.getByRole("status")).toContainText(/accepted/i);
  await expect(node.getByTestId("batch-state")).toHaveText("running");
});

test("output/add refusals are visible and Add sends only submitted valid JSON", async ({ page }) => {
  await enterTree(page);
  const node = page.locator('[data-batch-node="leaf"]');
  await node.getByRole("button", { name: "Output" }).click();
  let sent = await waitForWSSend(page, "batch_output");
  await sendMockWSMessage(page, { type: "batch_control_result", id: sent.id, payload: { sessionID: "child", rootID: "child-root", nodeID: "leaf", action: "batch_output", result: "output unavailable", ok: false } });
  await expect(node.getByRole("status")).toContainText("Refused: output unavailable");
  const addNode = page.locator('[data-batch-node="add-node"]');
  const input = addNode.getByRole("textbox", { name: "Items to add under add-node" });
  await input.fill("not json");
  await addNode.getByRole("button", { name: "Add" }).click();
  await expect(addNode.getByRole("status")).toContainText("JSON");
  const sentTypes = await page.evaluate(() => ((window as unknown as { __wsSent: { type: string }[] }).__wsSent).map((frame) => frame.type));
  expect(sentTypes).not.toContain("batch_add");
  await input.fill('[{"task":{"kind":"run_command","payload":{"tool":"run_command","input":{"command":"echo ok"}}}}]');
  await addNode.getByRole("button", { name: "Add" }).click();
  sent = await waitForWSSend(page, "batch_add");
  expect(sent.payload).toMatchObject({ sessionID: "child", rootID: "child-root", nodeID: "add-node", items: [{ task: { kind: "run_command" } }] });
  await sendMockWSMessage(page, { type: "batch_control_result", id: sent.id, payload: { sessionID: "child", rootID: "child-root", nodeID: "add-node", action: "batch_add", result: "unsupported", ok: false } });
  await expect(addNode.getByRole("status")).toContainText("Refused: unsupported");
});

test("drain has independent busy state and refusal notice", async ({ page }) => {
  await enterTree(page);
  await page.getByRole("button", { name: "Drain" }).click();
  const sent = await waitForWSSend(page, "batch_drain");
  await expect(page.getByRole("button", { name: "Draining…" })).toBeDisabled();
  await sendMockWSMessage(page, { type: "batch_control_result", id: sent.id, payload: { sessionID: "control-ui", action: "batch_drain", result: "not supported", ok: false } });
  await expect(page.getByTestId("batch-tree-message")).toContainText("Refused: not supported");
  await expect(page.getByRole("button", { name: "Drain" })).toBeEnabled();
});
