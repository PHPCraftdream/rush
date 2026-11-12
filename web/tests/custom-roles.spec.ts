import { test, expect, type Page } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeConfig, makeSession } from "./helpers/fixtures";
import type { CustomRole, CustomRolesState } from "../src/customRoleTypes";

const reader: CustomRole = { provider: "role-provider", model: "model-a", reasoning_effort: "high", tools: ["view"] };

function stateWith(global: Record<string, CustomRole> = {}, workspace: Record<string, CustomRole> = {}): CustomRolesState {
  return { global, workspace, effective: { ...global, ...workspace }, hasWorkspace: true };
}

async function openEditor(page: Page, state: CustomRolesState) {
  await setupMockWS(page);
  await page.route("/auth/check", (route) => route.fulfill({ status: 200, body: "OK" }));
  await page.goto("/");
  await sendMockWSMessage(page, { type: "sessions_list", payload: [makeSession({ ID: "role-editor-session", Title: "Role editor" })] });
  await sendMockWSMessage(page, { type: "config", payload: makeConfig({
    models: { smart: { Provider: "role-provider", Model: "model-a" } },
    providers: { "role-provider": { enabled: true, type: "openai-compat", models: [
      { id: "model-a", name: "Model A", reasoningLevels: ["low", "high"] },
      { id: "model-b", name: "Model B", reasoningLevels: ["max"] },
    ] } },
  }) });
  await page.getByTestId("header-more-button").click();
  await page.getByTestId("header-default-models-button").click();
  const request = await waitForWSSend(page, "get_custom_roles");
  await sendMockWSMessage(page, { type: "custom_roles", id: request.id, payload: state });
  const editor = page.getByTestId("custom-roles-editor");
  await expect(editor.getByLabel("Role name")).toBeEnabled();
  return editor;
}

async function acknowledge(page: Page, id: string | undefined, state: CustomRolesState) {
  const previousCount = await page.evaluate(() => ((window as unknown as Record<string, unknown>).__wsSent as unknown[]).length);
  await sendMockWSMessage(page, { type: "response", id, payload: { status: "ok" } });
  const next = await page.waitForFunction((offset: number) => {
    const sent = (window as unknown as Record<string, unknown>).__wsSent as Array<{ type: string; id?: string }>;
    return sent.slice(offset).find((message) => message.type === "get_custom_roles") ?? null;
  }, previousCount);
  const refresh = await next.jsonValue() as { type: string; id?: string };
  await next.dispose();
  await sendMockWSMessage(page, { type: "custom_roles", id: refresh.id, payload: state });
}

test.afterEach(async ({ page }, info) => {
  if (info.status === info.expectedStatus || page.isClosed()) return;
  const trace = await page.evaluate(() => {
    const recorded = window as unknown as Record<string, unknown>;
    return { sent: recorded.__wsSent, received: recorded.__wsReceived };
  }).catch(() => ({ unavailable: true }));
  await info.attach("custom-role-wire-trace", { body: JSON.stringify(trace, null, 2), contentType: "application/json" });
});

for (const name of ["constructor", "toString", "__proto__"]) {
  test(`creates prototype-named role ${name} in an empty scope`, async ({ page }) => {
    const editor = await openEditor(page, stateWith());
    await editor.getByLabel("Role name").fill(name);
    await editor.getByLabel("Provider", { exact: true }).fill("role-provider");
    await editor.getByLabel("Model", { exact: true }).fill("model-a");
    await editor.getByRole("button", { name: "Save role", exact: true }).click();
    const request = await waitForWSSend(page, "set_custom_role");
    expect(request.payload).toMatchObject({ scope: "global", name, provider: "role-provider", model: "model-a" });
  });
}

test("workspace model-only override of global constructor carries effective tools", async ({ page }) => {
  const constructorRole: CustomRole = { provider: "role-provider", model: "model-a", reasoning_effort: "high", tools: ["view", "mcp_*"] };
  const global = JSON.parse(JSON.stringify({ constructor: constructorRole })) as Record<string, CustomRole>;
  const editor = await openEditor(page, stateWith(global));
  await editor.getByLabel("Role scope").selectOption("workspace");
  await editor.getByRole("button", { name: "Edit role constructor", exact: true }).click();
  await expect(editor.getByText("constructor (global) role-provider/model-a")).toBeVisible();
  await expect(editor.getByLabel("Provider", { exact: true })).toHaveValue("role-provider");
  await expect(editor.getByLabel("Model", { exact: true })).toHaveValue("model-a");
  await expect(editor.getByLabel("Allowed tools")).toHaveValue("patterns");
  await expect(editor.getByLabel("Tool patterns (JSON array)")).toHaveValue('[\n  "view",\n  "mcp_*"\n]');
  await expect(editor.getByRole("button", { name: "Remove scoped definition", exact: true })).toBeDisabled();
  await editor.getByLabel("Model", { exact: true }).fill("model-b");
  await editor.getByRole("button", { name: "Save role", exact: true }).click();
  const request = await waitForWSSend(page, "set_custom_role");
  expect(request.payload).toMatchObject({ scope: "workspace", name: "constructor", model: "model-b", tools: ["view", "mcp_*"] });
});

for (const name of ["constructor", "toString", "__proto__"]) {
  test(`prototype-named scoped definitions are removable only when owned: ${name}`, async ({ page }) => {
    const role: CustomRole = { provider: "role-provider", model: "model-a", tools: ["view"] };
    const workspace = JSON.parse(JSON.stringify({ [name]: role })) as Record<string, CustomRole>;
    const editor = await openEditor(page, stateWith({}, workspace));
    await editor.getByLabel("Role scope").selectOption("workspace");
    await editor.getByRole("button", { name: `Edit role ${name}`, exact: true }).click();
    await expect(editor.getByRole("button", { name: "Remove scoped definition", exact: true })).toBeEnabled();
    await editor.getByRole("button", { name: "Remove scoped definition", exact: true }).click();
    const request = await waitForWSSend(page, "remove_custom_role");
    expect(request.payload).toEqual({ scope: "workspace", name });
  });
}

test("creates a role with explicit zero tools and the selected effort", async ({ page }) => {
  const editor = await openEditor(page, stateWith());
  await editor.getByLabel("Role name").fill("composer");
  await editor.getByLabel("Provider", { exact: true }).fill("role-provider");
  await editor.getByLabel("Model", { exact: true }).fill("model-a");
  await editor.getByLabel("Reasoning effort").selectOption("high");
  await editor.getByLabel("Allowed tools").selectOption("none");
  await editor.getByRole("button", { name: "Save role", exact: true }).click();
  const request = await waitForWSSend(page, "set_custom_role");
  expect(request.payload).toEqual({ scope: "global", name: "composer", provider: "role-provider", model: "model-a", reasoning_effort: "high", tools: [] });
  await acknowledge(page, request.id, stateWith({ composer: { provider: "role-provider", model: "model-a", reasoning_effort: "high", tools: [] } }));
  await expect(editor.getByRole("status")).toHaveText("Role saved.");
});

test("model-only editing does not overwrite a concurrent tools change", async ({ page }) => {
  const editor = await openEditor(page, stateWith({ reader }));
  await editor.getByRole("button", { name: "Edit role reader", exact: true }).click();
  await expect(editor.getByLabel("Allowed tools")).toHaveValue("patterns");
  await sendMockWSMessage(page, { type: "custom_roles", payload: stateWith({ reader: { ...reader, tools: ["view", "grep"] } }) });
  await editor.getByLabel("Model", { exact: true }).fill("model-b");
  await editor.getByRole("button", { name: "Save role", exact: true }).click();
  const request = await waitForWSSend(page, "set_custom_role");
  const payload = request.payload as Record<string, unknown>;
  expect(payload.model).toBe("model-b");
  expect(payload.tools).toBeUndefined();
  expect(payload.inherit_tools).toBeUndefined();
  await acknowledge(page, request.id, stateWith({ reader: { ...reader, model: "model-b", reasoning_effort: "", tools: ["view", "grep"] } }));
  await expect(editor.getByRole("status")).toHaveText("Role saved.");
});

test("explicit inheritance removes the tools field and preserves an error draft", async ({ page }) => {
  const editor = await openEditor(page, stateWith({ reader: { ...reader, think: true, tools: [] } }));
  await editor.getByRole("button", { name: "Edit role reader", exact: true }).click();
  await editor.getByLabel("Allowed tools").selectOption("inherit");
  await editor.getByRole("button", { name: "Save role", exact: true }).click();
  const request = await waitForWSSend(page, "set_custom_role");
  expect(request.payload).toMatchObject({ name: "reader", inherit_tools: true, reasoning_effort: "high", think: true });
  expect((request.payload as Record<string, unknown>).tools).toBeUndefined();
  const refusal = { type: "error", id: request.id, error: "change refused" };
  await sendMockWSMessage(page, refusal);
  await expect(editor.getByRole("alert")).toHaveText("change refused");
  await expect(editor.getByLabel("Role name")).toHaveValue("reader");
  await expect(editor.getByLabel("Allowed tools")).toHaveValue("inherit");
  await expect(editor.getByRole("button", { name: "Save role", exact: true })).toBeEnabled();
  await expect(editor.getByText("Role saved.", { exact: true })).toHaveCount(0);
});

test("removing a workspace definition reveals the global role", async ({ page }) => {
  const editor = await openEditor(page, stateWith({ reader }, { reader: { ...reader, model: "model-b", tools: [] } }));
  await editor.getByLabel("Role scope").selectOption("workspace");
  await editor.getByRole("button", { name: "Edit role reader", exact: true }).click();
  await editor.getByRole("button", { name: "Remove scoped definition", exact: true }).click();
  const request = await waitForWSSend(page, "remove_custom_role");
  expect(request.payload).toEqual({ scope: "workspace", name: "reader" });
  await acknowledge(page, request.id, stateWith({ reader }));
  await expect(editor.getByRole("status")).toContainText("lower-scope definition may now apply");
  await editor.getByRole("button", { name: "Edit role reader", exact: true }).click();
  await expect(editor.getByLabel("Model", { exact: true })).toHaveValue("model-a");
  await expect(editor.getByLabel("Allowed tools")).toHaveValue("patterns");
  await expect(editor.getByRole("button", { name: "Remove scoped definition", exact: true })).toBeDisabled();
});
