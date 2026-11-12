const EFFORT_LABELS: Record<string, string> = {
  none: "OFF",
  minimal: "MIN",
  low: "L",
  medium: "M",
  high: "H",
  xhigh: "X",
  max: "XX",
  ultra: "U",
};

/**
 * Short badge label of an effort level.
 *
 * @param effort - effort level
 */
export function effortLabel(effort: string): string {
  return EFFORT_LABELS[effort] ?? effort.toUpperCase();
}
