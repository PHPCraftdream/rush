import type { ProviderPeakHoursPolicy } from "../../types";
import { END_RE } from "./END_RE.ts";
import { HH_MM_RE } from "./HH_MM_RE.ts";
import { MAX_WINDOWS } from "./MAX_WINDOWS.ts";

// peakHoursProblems returns non-blocking UX-hint problems (malformed HH:MM,
// start == end, too many windows). The parent consumes its length for
// canSubmit; the editor also renders the strings inline as hints.

/**
 * Lists the problems of a peak-hours policy.
 *
 * @param value - the policy, or null when peak hours are off
 */
export function peakHoursProblems(value: ProviderPeakHoursPolicy | null): string[] {
  if (!value) return [];
  const problems: string[] = [];
  const windows = value.windows ?? [];
  if (windows.length > MAX_WINDOWS) problems.push(`At most ${MAX_WINDOWS} windows are allowed`);
  windows.forEach((w, i) => {
    const label = windows.length > 1 ? `Window ${i + 1}: ` : "";
    if (!HH_MM_RE.test(w.start) || !END_RE.test(w.end)) {
      problems.push(`${label}times must be in 24-hour HH:MM format (end may be 24:00)`);
    } else if (w.start === w.end) problems.push(`${label}start and end must differ`);
  });
  return problems;
}
