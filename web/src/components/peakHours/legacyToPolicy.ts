import type { PeakHoursWindowSpec, ProviderPeakHoursPolicy } from "../../types";

/** Either wire shape of a provider's peak hours: the policy, or the pre-#56 `{ start, end }` one. */
interface ILegacyPeak {
  start?: string;
  end?: string;
  message?: string;
  windows?: readonly PeakHoursWindowSpec[];
}

// legacyToPolicy maps the pre-#56 wire shape { start, end, message? } onto a
// policy so old data edits fine and round-trips back as a policy.

/**
 * Maps either wire shape of peak hours onto a policy.
 *
 * @param peak - the wire value
 */
export function legacyToPolicy(peak: ILegacyPeak | null | undefined): ProviderPeakHoursPolicy | null {
  if (!peak) return null;
  if (Array.isArray(peak.windows)) return peak as ProviderPeakHoursPolicy;
  if (peak.start && peak.end) {
    return { windows: [{ days: "", start: peak.start, end: peak.end }], message: peak.message ?? "" };
  }
  return null;
}
