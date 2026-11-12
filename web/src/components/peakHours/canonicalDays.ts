import { DAYS } from "./DAYS.ts";

// canonicalDays normalises a days spec to one of the wire-canonical forms:
// "" (every day), "mon-fri" (a contiguous mon..sun run, wrap allowed e.g.
// "fri-mon"), or a comma list in mon..sun order ("mon,wed,fri"). This is a
// pure string helper — no clock is consulted.

/**
 * Normalises a days spec to its wire-canonical form.
 *
 * @param days - the spec to normalise
 */
export function canonicalDays(days: string): string {
  const set = new Set<string>();
  for (const token of days.split(",")) {
    const t = token.trim().toLowerCase();
    if (!t) continue;
    const range = t.split("-");
    if (range.length === 2 && DAYS.includes(range[0]) && DAYS.includes(range[1])) {
      // Expand a (possibly wrapping) contiguous run into its day set.
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
  if (set.size === 0 || set.size === 7) return "";
  // Contiguous (with wrap) runs collapse to "x-y"; anything else lists in
  // mon..sun order.
  const ordered = DAYS.filter((d) => set.has(d));
  const startIdx = DAYS.indexOf(ordered[0]);
  let contiguous = true;
  for (let k = 0; k < ordered.length; k++) {
    if (ordered[k] !== DAYS[(startIdx + k) % 7]) {
      contiguous = false;
      break;
    }
  }
  if (contiguous) {
    return ordered.length === 1 ? ordered[0] : `${ordered[0]}-${ordered[ordered.length - 1]}`;
  }
  return ordered.join(",");
}
