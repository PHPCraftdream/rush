/** Either wire shape of a provider's peak hours, as far as the summary needs. */
interface IPeakSummarySource {
  windows?: readonly { start: string; end: string }[];
  start?: string;
  end?: string;
}

/**
 * Whether a window has both its times.
 *
 * @param w - the window
 */
const isComplete = (w: { start: string; end: string }): boolean => !!(w.start && w.end);

/**
 * A window as "start–end".
 *
 * @param w - the window
 */
const windowText = (w: { start: string; end: string }): string => `${w.start}–${w.end}`;

// peakSummaryText renders the wire summary of either a policy (windows
// list) or the legacy { start, end } shape, joined with ", ". Pure string
// helper — used by ProviderRow where the peakHours union must narrow
// without a type cast.

/**
 * Renders the wire summary of a peak-hours value.
 *
 * @param peak - the policy or legacy value
 */
export function peakSummaryText(peak: IPeakSummarySource | null | undefined): string {
  if (peak?.windows && peak.windows.length > 0) {
    return peak.windows.filter(isComplete).map(windowText).join(", ");
  }
  if (peak?.start && peak?.end) return `${peak.start}–${peak.end}`;
  return "";
}
