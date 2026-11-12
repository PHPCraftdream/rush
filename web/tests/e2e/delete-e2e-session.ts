import { Page } from '@playwright/test';

/** Browser-side: asks the server to delete the session with the given ID. */
function sendDeleteSession(id: string) {
  const ws = ((window as unknown) as Record<string, unknown>)["__mockWS"] as WebSocket;
  if (ws && ws.readyState === 1) {
    ws.send(JSON.stringify({ type: "delete_session", payload: { sessionID: id } }));
  }
}

/**
 * Delete a test session
 */
export async function deleteE2ESession(page: Page, sessionID: string): Promise<void> {
  await page.evaluate(sendDeleteSession, sessionID);
  await page.waitForTimeout(500);
  console.log("[E2E] Deleted session:", sessionID);
}
