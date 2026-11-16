import type { Page } from "@playwright/test";

/** Browser-side: empties the __wsSent log. */
function resetWSSent() {
  ((window as unknown) as Record<string, unknown>)["__wsSent"] = [];
}

/**
 * Clears the sent messages array. Useful to wait for NEW messages only.
 */
export async function clearWSSent(page: Page) {
  await page.evaluate(resetWSSent);
}
