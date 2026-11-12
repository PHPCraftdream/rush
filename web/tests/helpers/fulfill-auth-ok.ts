import type { Route } from "@playwright/test";

/** Route handler answering the /auth/check probe with 200 OK. */
export function fulfillAuthOk(route: Route) {
  return route.fulfill({ status: 200, body: "OK" });
}
