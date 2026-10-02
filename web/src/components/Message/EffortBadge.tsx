// Reasoning-effort tier badge for model-selected levels.
// Pure code move from the former components/Message.tsx.

import { memo } from "react";
import { effortLabel } from "../../effort";

// Displays the effort recorded on a message; unknown effort names render as
// their own uppercase value instead of an ambiguous question mark.

export const EffortBadge = memo(function EffortBadge({ effort, extraClass = "" }: { effort: string | undefined; extraClass?: string }) {
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
