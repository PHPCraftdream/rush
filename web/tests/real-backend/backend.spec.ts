import { expect, test } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';

const controller = 'http://127.0.0.1:60986';

type State = {
  owner: string;
  root: string;
  gateDir: string;
  token: string;
  children: string[];
  noticeNode: string;
  suppressedNode: string;
};

/** Reads the fixture controller's current state. */
async function state(request: APIRequestContext): Promise<State> {
  const r = await request.get(`${controller}/fixture/state`);
  expect(r.ok()).toBeTruthy();
  return r.json() as Promise<State>;
}

/** Resets the fixture and returns the fresh batch root and owner. */
async function setup(request: APIRequestContext) {
  const r = await request.post(`${controller}/fixture/reset`);
  expect(r.ok(), await r.text()).toBeTruthy();
  return r.json() as Promise<{ root: string; owner: string }>;
}

/** Releases one gated child task in the fixture. */
async function release(request: APIRequestContext, id: string) {
  expect((await request.post(`${controller}/fixture/release/${encodeURIComponent(id)}`)).status()).toBe(204);
}

/** Asks the fixture to project its pending notices. */
async function projectNotices(request: APIRequestContext) {
  const r = await request.post(`${controller}/fixture/notices`);
  expect(r.status(), await r.text()).toBe(204);
}

/** Race marker: the login form appeared. */
function loginMarker() {
  return 'login';
}

/** Race marker: the connected shell appeared. */
function shellMarker() {
  return 'shell';
}

/** Race marker: neither appeared in time. */
function noMarker() {
  return 'none';
}

/** Turns a rejected wait into the none marker. */
function orNone(p: Promise<string>) {
  return p.catch(noMarker);
}

/**
 * After a reload the auth cookie persists, so the app may already be
 * connected: wait for EITHER the password input OR the connected shell.
 */
async function openApp(page: any, request: APIRequestContext) {
  const s = await state(request);
  await page.goto('/');
  const pw = page.locator('input[type="password"]');
  const sel = page.getByTestId('live-work-tab-tasks');
  const state2 = await Promise.race([
    pw.waitFor({ state: 'visible', timeout: 5000 }).then(loginMarker),
    sel.waitFor({ state: 'visible', timeout: 5000 }).then(shellMarker),
  ].map(orNone));
  if (state2 === 'login') {
    await pw.fill(s.token);
    await page.getByRole('button', { name: 'Connect' }).click();
  }
  await expect(sel).toBeVisible();
  return s;
}

test('real runtime hierarchy survives reload; Add and subtree Stop preserve siblings', async ({ page, request }) => {
  const batch = await setup(request);
  const fixture = await openApp(page, request);
  await page.getByTestId('live-work-tab-batches').click();
  await expect(page.locator(`[data-batch-root="${batch.root}"]`)).toBeVisible();
  await expect(page.locator(`[data-batch-node="${batch.root}.1.1"]`)).toBeVisible();
  await expect(page.locator(`[data-batch-node="${batch.root}.1.2"]`)).toBeVisible();
  const addBox = page.locator(`[data-batch-add="${batch.root}.1"]`);
  await addBox.fill(JSON.stringify([{
    task: {
      kind: 'generic',
      payload: { tool: 'run_command', input: { program: 'echo', args: ['added fixture'], description: 'fixture add' } },
    },
  }]));
  await page.getByTestId(`batch-add-${batch.root}.1`).click();
  await expect(page.locator(`[data-batch-node-message="${batch.root}.1"]`)).toContainText(/added/i);
  const target = page.locator(`[data-batch-node="${batch.root}.1.1"]`);
  await target.getByRole('button', { name: 'Stop' }).click();
  await expect(target.locator('[data-test-id="batch-state"]')).toContainText(/cancelling|cancelled/i);
  const sibling = page.locator(`[data-batch-node="${batch.root}.1.2"]`);
  await expect(sibling).toBeVisible();
  await expect(sibling.locator('[data-test-id="batch-state"]')).toHaveText('running');
  await projectNotices(request);
  await page.reload();
  const current = await openApp(page, request);
  await page.getByTestId('live-work-tab-batches').click();
  await expect(page.locator(`[data-batch-root="${batch.root}"]`)).toBeVisible();
  await expect(page.locator(`[data-batch-node="${fixture.noticeNode}"]`)).toContainText('Delayed notice');
  await expect(page.locator(`[data-batch-node="${fixture.suppressedNode}"]`)).toContainText('Notice suppressed');
  const start = Date.now();
  await page.getByRole('button', { name: 'Drain' }).click();
  await expect(page.getByTestId('batch-tree-message')).toContainText(
    /no public session-scoped automatic reaction drain API/i,
  );
  expect(Date.now() - start).toBeLessThan(3000);
  await Promise.all(current.children.map((id) => release(request, id)));
});
