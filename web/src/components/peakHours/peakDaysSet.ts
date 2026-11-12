import { DAYS } from "./DAYS.ts";

// peakDaysSet expands a days spec into the full set of days it covers.
// "" means every day; each comma token is a single day or a contiguous
// range that may wrap ("fri-mon" walks forward through DAYS modulo 7).
// Unknown tokens are ignored so malformed wire data can't crash the editor.

/**
 * Expands a days spec into the set of days it covers.
 *
 * @param days - the spec: "", "mon-fri", "fri-mon" or a comma list
 */
export function peakDaysSet(days: string): Set<string> {
  const set = new Set<string>();
  if (!days) {
    DAYS.forEach((d) => set.add(d));
    return set;
  }
  for (const token of days.split(",")) {
    const t = token.trim().toLowerCase();
    const range = t.split("-");
    if (range.length === 2 && DAYS.includes(range[0]) && DAYS.includes(range[1])) {
      let i = DAYS.indexOf(range[0]);
      const j = DAYS.indexOf(range[1]);
      while (true) {
        set.add(DAYS[i]);
        if (i === j) break;
        i = (i + 1) % 7;
      }
    } else if (DAYS.includes(t)) {
      set.add(t);
    }
  }
  return set;
}
