import type { Page } from "@playwright/test";
import type { MockWSMessage } from "./mock-ws";

/** Browser-side: true once the mock WS is open and the app has set onmessage. */
function isMockWSReady() {
  const ws = ((window as unknown) as Record<string, unknown>)["__mockWS"] as {
    readyState: number;
    onmessage: unknown;
  } | null;
  return ws !== null && ws.readyState === 1 && typeof ws.onmessage === "function";
}

/** Browser-side: records the serialized message in __wsReceived and delivers it to the app. */
function deliverToMockWS(data: string) {
  const ws = ((window as unknown) as Record<string, unknown>)["__mockWS"] as {
    onmessage: ((ev: MessageEvent) => void) | null;
  } | null;
  // Track received messages
  const msgObj = JSON.parse(data);
  ((window as unknown) as Record<string, unknown>)["__wsReceived"] = ((window as unknown) as Record<string, unknown>)["__wsReceived"] as unknown[] ?? [];
  ((window as unknown) as Record<string, unknown>)["__wsReceived"] = (((window as unknown) as Record<string, unknown>)["__wsReceived"] as unknown[]).concat(msgObj);
  if (ws?.onmessage) ws.onmessage(new MessageEvent("message", { data }));
}

/**
 * Injects a server→client message via the mock WS.
 * Waits up to 10 s for the mock WS to be open.
 */
export async function sendMockWSMessage(page: Page, msg: MockWSMessage) {
  await page.waitForFunction(isMockWSReady, { timeout: 10_000 });

  await page.evaluate(deliverToMockWS, JSON.stringify(msg));

  await page.waitForTimeout(50);
}
