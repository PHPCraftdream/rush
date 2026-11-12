import { Page } from '@playwright/test';

/** Browser-side: sends a send_message frame for the given session. */
function sendChatMessage({ sid, msg }: { sid: string; msg: string }) {
  const ws = ((window as unknown) as Record<string, unknown>)["__mockWS"] as WebSocket;
  if (ws && ws.readyState === 1) {
    ws.send(JSON.stringify({ type: "send_message", payload: { sessionID: sid, content: msg } }));
  }
}

/**
 * Send a message to the AI and wait for response
 */
export async function sendMessage(page: Page, sessionID: string, content: string): Promise<void> {
  await page.evaluate(sendChatMessage, { sessionID, content });
  console.log("[E2E] Sent message:", content);
}
