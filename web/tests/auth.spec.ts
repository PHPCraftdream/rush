import { test, expect, type Route } from "@playwright/test";

/** Route handler answering with 401. */
function fulfillUnauthorized(route: Route) {
  return route.fulfill({ status: 401 });
}

test("shows login page initially", async ({ page }) => {
  // Intercept auth check to return not authed
  await page.route("/auth/check", fulfillUnauthorized);
  await page.goto("/");
  await expect(page.locator('input[type="password"]')).toBeVisible();
});

test("shows Connect button on login page", async ({ page }) => {
  await page.route("/auth/check", fulfillUnauthorized);
  await page.goto("/");
  await expect(page.getByText("Connect")).toBeVisible();
});

/** Route handler rejecting the token with 401 Unauthorized. */
function fulfillInvalidToken(route: Route) {
  return route.fulfill({ status: 401, body: "Unauthorized" });
}

test("shows error for invalid token", async ({ page }) => {
  await page.route("/auth/check", fulfillUnauthorized);
  await page.route("/auth", fulfillInvalidToken);
  await page.goto("/");
  await page.locator('input[type="password"]').fill("wrong-token");
  await page.getByText("Connect").click();
  await expect(page.getByText("Invalid token")).toBeVisible();
});

test("Connect button is disabled when token is empty", async ({ page }) => {
  await page.route("/auth/check", fulfillUnauthorized);
  await page.goto("/");
  const btn = page.getByText("Connect");
  await expect(btn).toBeDisabled();
});

/** Route handler accepting the token with 200 OK. */
function fulfillValidToken(route: Route) {
  return route.fulfill({ status: 200, body: "OK" });
}

test("navigates to app on valid token", async ({ page }) => {
  await page.route("/auth/check", fulfillUnauthorized);
  await page.route("/auth", fulfillValidToken);
  // Block WS connection from erroring out visibly
  await page.goto("/");
  await page.locator('input[type="password"]').fill("valid-token");
  await page.getByText("Connect").click();
  // After successful auth, login page should disappear
  await expect(page.locator('input[type="password"]')).not.toBeVisible({ timeout: 3000 });
});
