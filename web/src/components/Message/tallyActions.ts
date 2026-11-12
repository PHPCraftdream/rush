// One-line tool tally for a tool activity group header.

import type { ActionItem } from "./ActionRow";

/**
 * Orders tally entries by descending count.
 *
 * @param a - one [name, count] entry
 * @param b - another [name, count] entry
 */
const byCountDesc = (a: [string, number], b: [string, number]): number => b[1] - a[1];

/**
 * Formats one tally entry as "<count> <name>".
 *
 * @param entry - a [name, count] entry
 */
const formatTallyEntry = ([name, count]: [string, number]): string => `${count} ${name}`;

/**
 * Summarises the rows as "3 bash · 2 read · 1 thinking": the four most-used
 * tools by count, then the thinking-row count.
 *
 * @param actions - the group's rows
 */
export function tallyActions(actions: readonly ActionItem[]): string {
  const counts = new Map<string, number>();
  let thinkingCount = 0;
  for (const a of actions) {
    if (a.kind === "thinking") { thinkingCount++; continue; }
    const n = a.callPart?.Name ?? a.resultPart?.Name ?? "tool";
    counts.set(n, (counts.get(n) ?? 0) + 1);
  }
  const parts = [...counts.entries()].sort(byCountDesc).slice(0, 4).map(formatTallyEntry);
  if (thinkingCount > 0) parts.push(`${thinkingCount} thinking`);
  return parts.join(" · ");
}
