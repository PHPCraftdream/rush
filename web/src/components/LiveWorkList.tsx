// Commands / Agents tab body for LiveWorkPanel (tasks #1059/#1058): a list
// of async-work items sourced from the server's durable async_jobs snapshot,
// each jumping the chat to its tool call on click. A running item shows a
// ticking elapsed label; a finished one shows a distinct status label --
// stopped by the user, killed, timed out, failed, and interrupted must not
// look alike (#1058).

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

// Ticks a re-render once a second so the elapsed label keeps counting -- but
// only for a still-running item; a terminal row's label never changes, so it
// gets no interval at all.
function useElapsedLabel(item: LiveWorkItem): string {
  const running = !item.status || item.status === "running";
  const [, bump] = useState(0);
  useEffect(() => {
    if (!running) return;
    const id = window.setInterval(() => bump((n) => n + 1), 1000);
    return () => window.clearInterval(id);
  }, [running]);
  if (!running) return formatElapsed(item.finishedAt || item.lastActivityAt || item.startedAt);
  return formatElapsed(item.startedAt);
}

// statusLabel maps a durable async_jobs.state (+ notice_kind reason) to the
// short label the row shows. Cancelled splits by reason: "job_kill" is a
// user-killed command, "session_cancel" a session-level stop -- both
// deliberately distinct from timed_out and failed (#1058).
export function statusLabel(item: LiveWorkItem): string {
  switch (item.status) {
    case "running":
    case "":
    case undefined:
      return "";
    case "completed":
      return "done";
    case "failed":
      return "failed";
    case "timed_out":
      return "timed out";
    case "interrupted":
      return "interrupted";
    case "cancelled":
      return item.reason === "session_cancel" ? "stopped by user" : "killed";
    default:
      return item.status;
  }
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
  const elapsed = useElapsedLabel(item);
  const status = statusLabel(item);
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
        {status ? (
          <span data-test-id="live-work-row-status" className="text-text-subtle text-xs font-mono shrink-0">
            {status}
          </span>
        ) : (
          <span className="text-text-subtle text-xs font-mono tabular-nums shrink-0">{elapsed}</span>
        )}
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
