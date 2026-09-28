// Commands / Agents tab body for LiveWorkPanel (task #1059): a flat list of
// in-flight items, each jumping the chat to its tool call on click.

import { useEffect, useState } from "react";
import type { LiveWorkItem } from "../types";
import { requestExpandToolCall } from "../store_livework";

function formatElapsed(startedAt: number): string {
  const secs = Math.max(0, Math.floor((Date.now() - startedAt) / 1000));
  if (secs < 60) return `${secs}s`;
  const m = Math.floor(secs / 60);
  const s = secs % 60;
  if (m < 60) return `${m}m ${s}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

// Ticks a re-render once a second so the elapsed label keeps counting while
// an item is visible -- these are "running right now" items by definition,
// so there is no "finished, stop ticking" state to reach.
function useElapsedLabel(startedAt: number): string {
  const [, bump] = useState(0);
  useEffect(() => {
    const id = window.setInterval(() => bump((n) => n + 1), 1000);
    return () => window.clearInterval(id);
  }, []);
  return formatElapsed(startedAt);
}

// jumpToToolCall requests the accordion holding `toolCallID` to expand
// (store-driven signal, see useExpandToolCallSignal), then waits two
// animation frames before querying the DOM: the request only updates a
// nanostore, and the row's actual mount (ToolActivityGroup re-rendering
// itself open, then mounting the matching ActionRow/SubAgentBlock) lands a
// commit or two later -- one frame is not reliably enough to observe it.
// When the anchor still isn't found (message outside the loaded window, or
// this session's transcript was never loaded), calls onNotFound instead of
// failing silently.
function jumpToToolCall(toolCallID: string, onNotFound: () => void) {
  requestExpandToolCall(toolCallID);
  requestAnimationFrame(() => {
    requestAnimationFrame(() => {
      const el = document.querySelector(`[data-tool-call-id="${CSS.escape(toolCallID)}"]`);
      if (el) {
        el.scrollIntoView({ behavior: "smooth", block: "center" });
      } else {
        onNotFound();
      }
    });
  });
}

function LiveWorkRow({ item }: { item: LiveWorkItem }) {
  const elapsed = useElapsedLabel(item.startedAt);
  const [notFound, setNotFound] = useState(false);

  function onClick() {
    setNotFound(false);
    jumpToToolCall(item.toolCallID, () => {
      setNotFound(true);
      window.setTimeout(() => setNotFound(false), 4000);
    });
  }

  return (
    <div data-test-id="live-work-row">
      <button
        type="button"
        data-test-id="live-work-row-jump"
        onClick={onClick}
        title={item.title || item.toolName}
        className="w-full flex items-center gap-2 px-2 py-1.5 text-left hover:bg-base-overlay/50 transition-colors rounded"
      >
        <span className="text-mauve font-semibold text-sm shrink-0">{item.toolName}</span>
        <span className="text-text font-mono text-sm truncate flex-1 min-w-0" style={{ fontSize: "var(--chat-font-size)" }}>
          {item.title || "—"}
        </span>
        <span className="text-text-subtle text-xs font-mono tabular-nums shrink-0">{elapsed}</span>
      </button>
      {notFound && (
        <p data-test-id="live-work-row-hint" className="px-2 pb-1 text-[11px] text-text-subtle">
          Not in the loaded conversation — try scrolling manually.
        </p>
      )}
    </div>
  );
}

export function LiveWorkList({ items, emptyLabel }: { items: LiveWorkItem[]; emptyLabel: string }) {
  return (
    <div data-test-id="live-work-list" className="px-2 py-2">
      {items.length === 0 ? (
        <p className="text-text-muted px-2 py-1" style={{ fontSize: "var(--chat-font-size)" }}>{emptyLabel}</p>
      ) : (
        items.map((item) => <LiveWorkRow key={item.toolCallID} item={item} />)
      )}
    </div>
  );
}
