import type { ProviderPeakHoursPolicy } from "../../types";

/** A fresh policy: one every-day 09:00-18:00 window and no message. */
export function defaultPolicy(): ProviderPeakHoursPolicy {
  return { windows: [{ days: "", start: "09:00", end: "18:00" }], message: "" };
}
