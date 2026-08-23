// Live-elapsed / final duration badge for assistant messages.
// Pure code move from the former components/Message.tsx.

import { useState, useEffect, useMemo, memo } from "react";
import { useStore } from "@nanostores/react";
import type { Message as Msg } from "../../types";
import { $busySessions } from "../../store";
import { isTerminallyFinished } from "./textParts";

function formatDuration(s: number) {
  if (s < 60) return `${s.toFixed(1)}s`;
  return `${Math.floor(s / 60)}m ${Math.floor(s % 60)}s`;
}

// ── Leaf components ───────────────────────────────────────────────────────────

export const DurationBadge = memo(function DurationBadge({ message }: { message: Msg }) {
  const isFinished = useMemo(() => isTerminallyFinished(message.Parts), [message.Parts]);
  const busy = useStore($busySessions);
  const isLive = !isFinished && busy.has(message.SessionID);
  const [elapsed, setElapsed] = useState(0);

  useEffect(() => {
    if (!isLive) return;
    const start = message.CreatedAt;
    const tick = () => setElapsed(Date.now() / 1000 - start);
    tick();
    const id = setInterval(tick, 100);
    return () => clearInterval(id);
  }, [isLive, message.CreatedAt]);

  const duration = isFinished || !isLive ? message.UpdatedAt - message.CreatedAt : elapsed;
  if (duration < 0.5) return null;
  return <span className="text-xs text-text-subtle font-mono tabular-nums">{formatDuration(duration)}</span>;
});
