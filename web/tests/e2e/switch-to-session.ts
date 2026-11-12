import { Page } from '@playwright/test';
import { E2E_PREFIX } from "./e2e-prefix";

/**
 * Switch to a session by clicking on it
 */
export async function switchToSession(page: Page, title: string): Promise<void> {
  await page.getByText(`${E2E_PREFIX}${title}`).first().click();
  await page.waitForTimeout(500);
  console.log("[E2E] Switched to session:", title);
}
