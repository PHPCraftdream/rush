/**
 * Picks the text colour class for a context-window fill percentage.
 *
 * @param pct - percent of the context window used
 */
export function pctColor(pct: number): string {
  if (pct >= 85) return "text-red";
  if (pct >= 60) return "text-yellow";
  return "text-green";
}
