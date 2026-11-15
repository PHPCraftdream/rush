import type { Page } from "@playwright/test";
import type { MockWSMessage } from "./mock-ws";

/** Browser-side: the most recent sent frame of the given type, or null. */
function lastSentOfType(t: string) {
  const sent = (((window as unknown) as Record<string, unknown>)["__wsSent"] as Array<{ type: string }>) ?? [];
  // Find LAST matching message (most recent)
  for (let i = sent.length - 1; i >= 0; i--) {
    if (sent[i].type === t) {
      return sent[i];
    }
  }
  return null;
}

/**
 * Polls until the page sends a WS command of the given type.
 * Returns the LAST matching message (most recent), not the first.
 */
export async function waitForWSSend(
  page: Page,
  type: string,
  timeout = 5_000
): Promise<MockWSMessage> {
  const handle = await page.waitForFunction(lastSentOfType, type, { timeout });
  return (await handle.jsonValue()) as MockWSMessage;
}
