// Token usage + cache efficiency badge for assistant messages.
// Pure code move from the former components/Message.tsx.

import { memo } from "react";
import type { MessageUsage } from "../../types";

// formatTokens renders a token count compactly (12345 -> "12.3k").
function formatTokens(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`;
  return `${(n / 1_000_000).toFixed(2)}M`;
}

// UsageBadge shows this message's own token accounting and cache efficiency.
//
// Two rules it must not break, both inherited from the backend's design:
//   * CacheHitRatio is null when the provider does not report caching. Show
//     "cache n/a", never "cache 0%" - a fabricated zero is indistinguishable
//     from a genuine cache miss.
//   * Estimated usage was derived from message lengths because the provider
//     sent none. Mark it, so an approximation is not read as a measurement.
export const UsageBadge = memo(function UsageBadge({ usage }: { usage: MessageUsage | undefined }) {
  if (!usage) return null;

  const cache =
    usage.CacheHitRatio === null
      ? "cache n/a"
      : `cache ${(usage.CacheHitRatio * 100).toFixed(0)}%`;

  const parts = [
    `Prompt ${usage.PromptTokens.toLocaleString()} = ${usage.InputTokens.toLocaleString()} fresh + ${usage.CacheReadTokens.toLocaleString()} cache-read + ${usage.CacheCreationTokens.toLocaleString()} cache-write`,
    `Output ${usage.OutputTokens.toLocaleString()}`,
  ];
  if (usage.CostUSD > 0) parts.push(`Cost $${usage.CostUSD.toFixed(4)}`);
  if (usage.CacheHitRatio === null) parts.push("This provider does not report prompt caching.");
  if (usage.Estimated) parts.push("ESTIMATED: the provider sent no usage; counts were derived from message lengths.");
  const title = parts.join("\n");

  return (
    <span
      className="text-[10px] text-text-muted font-mono flex items-center gap-1"
      title={title}
      data-test-id="message-usage-badge"
    >
      <span>{`↓${formatTokens(usage.PromptTokens)}`}</span>
      <span>{`↑${formatTokens(usage.OutputTokens)}`}</span>
      <span className={usage.CacheHitRatio === null ? "opacity-60" : ""}>{cache}</span>
      {usage.Estimated && <span title="Estimated, not reported by the provider">~</span>}
    </span>
  );
});
