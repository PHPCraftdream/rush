import type { ProviderPeakHoursPolicy } from "../../types";

/**
 * A window as "start–end".
 *
 * @param w - the window
 */
const windowText = (w: { start: string; end: string }): string => `${w.start}–${w.end}`;

/**
 * Joins every window of a policy as "start–end, ...".
 *
 * @param value - the policy, if any
 */
export function peakSummary(value: ProviderPeakHoursPolicy | null): string {
  return (value?.windows ?? []).map(windowText).join(", ");
}
