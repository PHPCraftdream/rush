import { canonicalDays } from "./canonicalDays.ts";
import { peakDaysSet } from "./peakDaysSet.ts";

/**
 * Flips one day in a days spec.
 *
 * Expands the CURRENT spec (not the canonical form) so ranges like "mon-fri"
 * toggle correctly instead of being re-expanded from a lossy list. A window
 * must keep at least one day — dropping the last day would leave a window with
 * no active time at all, so the previous set is kept instead.
 *
 * @param days - the current spec
 * @param day - the day to flip
 */
export function toggleDay(days: string, day: string): string {
  const set = peakDaysSet(days);
  if (set.has(day) && set.size === 1) return canonicalDays([...set].join(","));
  if (set.has(day)) set.delete(day);
  else set.add(day);
  return canonicalDays([...set].join(","));
}
