/**
 * Formats a token count compactly: 950, 1.2k, 3.4M.
 *
 * @param n - the token count
 */
export function formatTokens(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "k";
  return String(n);
}
