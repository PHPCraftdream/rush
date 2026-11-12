// Reasoning-effort tier badge for model-selected levels.

import { memo } from "react";
import { effortLabel } from "../../effort";

/** Props of the effort badge. */
interface IEffortBadgeProps {
  effort: string | undefined;
  extraClass?: string;
}

/**
 * Displays the effort recorded on a message; unknown effort names render as
 * their own uppercase value instead of an ambiguous question mark.
 */
export const EffortBadge = memo(function EffortBadge({ effort, extraClass = "" }: IEffortBadgeProps) {
  if (!effort) return null;
  const letter = effortLabel(effort);
  return (
    <span
      className={`px-1 py-0.5 rounded bg-base-subtle text-text-muted font-mono text-[10px] ${extraClass}`}
      title={`Reasoning effort: ${effort}`}
    >
      [{letter}]
    </span>
  );
});
