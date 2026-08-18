// Reasoning-effort tier badge ([L]/[M]/[H]/[X]/[XX]).
// Pure code move from the former components/Message.tsx.

import { memo } from "react";

// EffortBadge renders the model's reasoning-effort tier in square-bracket form
// next to the model name: [L] / [M] / [H] / [X] / [XX] for low/medium/high/xhigh/max.
// Shown unconditionally (no provider gate) so GLM/zai messages get their tier
// too — operators routinely run GLM at high vs max and want to tell them apart
// at a glance. Returns null when effort is unknown so the layout doesn't carry
// an empty bracket.

export const EffortBadge = memo(function EffortBadge({ effort, extraClass = "" }: { effort: string | undefined; extraClass?: string }) {
  if (!effort) return null;
  const letter = effort === "low" ? "L" : effort === "medium" ? "M" : effort === "high" ? "H" : effort === "xhigh" ? "X" : effort === "max" ? "XX" : "?";
  return (
    <span
      className={`px-1 py-0.5 rounded bg-base-subtle text-text-muted font-mono text-[10px] ${extraClass}`}
      title={`Reasoning effort: ${effort}`}
    >
      [{letter}]
    </span>
  );
});
