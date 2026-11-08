import { useEffect, useRef, useState } from "react";
import { fetchPeakHoursStatus } from "../store";
import type { ProviderPeakHoursStatus } from "../types";

// ── Backend-authoritative peak-hours status badge (task #56) ─────────────────
//
// Replaces the old row badge that decided "in peak" from the browser's local
// clock. All state (inPeak/paused/blocked) and every timestamp come from the
// server's get_peak_hours_status reply; the browser only FORMATS the strings.
//
// Refresh policy — bounded, no constant global polling:
//   1. Fetch on mount / providerId change.
//   2. After each fetch, find the next relevant boundary = the earliest
//      FUTURE timestamp among reopensAt / pausedUntil / nextTransition. If
//      one exists within 24h, schedule a single setTimeout refetch ~1s AFTER
//      that boundary (capped at 24h) so the badge flips at the transition
//      instead of the next poll.
//   3. Additionally ONE bounded fallback interval refetches every 120s while
//      mounted.
// All timers are cleared on unmount and on providerId change. Date.now() is
// used ONLY to schedule these refetches — never to decide peak/blocked state.

const BOUNDARY_CAP_MS = 24 * 60 * 60 * 1000;
const FALLBACK_INTERVAL_MS = 120 * 1000;

function fmt(ts?: string): string {
  return ts ? new Date(ts).toLocaleString() : "—";
}

export function PeakHoursStatus({
  providerId,
  configured,
  summary,
}: {
  providerId: string;
  // False when the provider has no peakHours configured: render nothing so
  // the row keeps "no badge" semantics (specs assert badge count 0).
  configured?: boolean;
  // Compact wire summary from ProviderInfo.peakHours (e.g. "09:00–18:00"),
  // shown on the badge itself; detailed state lives on the status line.
  summary?: string;
}) {
  const [status, setStatus] = useState<ProviderPeakHoursStatus | null>(null);
  // "unknown" while the first fetch is in flight or after an error.
  const [unknown, setUnknown] = useState(true);
  const boundaryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const fallbackTimerRef = useRef<ReturnType<typeof setInterval> | null>(null);
  // Exposes the current providerId's fetch to the retry button (re-created
  // per effect run, so it always refetches the right provider).
  const refreshRef = useRef<() => void>(() => {});

  useEffect(() => {
    let disposed = false;
    setUnknown(true);
    setStatus(null);

    function clearTimers() {
      if (boundaryTimerRef.current !== null) clearTimeout(boundaryTimerRef.current);
      if (fallbackTimerRef.current !== null) clearInterval(fallbackTimerRef.current);
      boundaryTimerRef.current = null;
      fallbackTimerRef.current = null;
    }

    // Schedule the boundary refetch ~1s after the earliest future boundary
    // (≤24h). This updates the badge AT the transition instead of polling
    // globally; Date.now() only schedules the fetch, state stays server-owned.
    function scheduleBoundary(s: ProviderPeakHoursStatus | null) {
      if (boundaryTimerRef.current !== null) clearTimeout(boundaryTimerRef.current);
      boundaryTimerRef.current = null;
      if (!s) return;
      const candidates = [s.reopensAt, s.pausedUntil, s.nextTransition].filter(Boolean).map((t) => new Date(t as string).getTime());
      const future = candidates.filter((t) => !Number.isNaN(t) && t > Date.now());
      if (future.length === 0) return;
      const diff = Math.min(...future) - Date.now() + 1000;
      if (diff > BOUNDARY_CAP_MS) return;
      boundaryTimerRef.current = setTimeout(() => { if (!disposed) refresh(); }, diff);
    }

    async function refresh() {
      try {
        const reply = await fetchPeakHoursStatus(providerId);
        if (disposed) return;
        const mine = reply.statuses.find((s) => s.providerId === providerId) ?? null;
        setStatus(mine);
        setUnknown(false);
        scheduleBoundary(mine);
      } catch {
        if (disposed) return;
        setUnknown(true);
      }
    }

    refresh();
    refreshRef.current = refresh;
    // Single bounded fallback refetch while mounted.
    fallbackTimerRef.current = setInterval(() => { if (!disposed) refresh(); }, FALLBACK_INTERVAL_MS);

    return () => {
      disposed = true;
      clearTimers();
    };
  }, [providerId]);

  if (configured === false) return null;

  const blocked = !!status?.blocked;
  const paused = !!status?.paused;
  const state = unknown ? "unknown" : blocked ? "blocked" : "open";
  const title = unknown
    ? "Peak-hours status unknown"
    : paused
      ? `Paused until ${fmt(status?.pausedUntil)}`
      : blocked
        ? `Blocked by peak-hours window${status?.reopensAt ? ` · reopens ${fmt(status.reopensAt)}` : ""}`
        : "Outside peak-hours window";

  return (
    <>
      <span
        className="inline-flex items-center gap-1 text-[11px] font-mono bg-accent/10 border border-accent/20 rounded-full px-2 py-0.5"
        data-test-id="provider-peak-badge"
        data-peak-state={state}
        title={title}
      >
        {summary || "peak hours"}
        {!unknown && (blocked || paused) && (
          <span className="inline-block w-1.5 h-1.5 rounded-full bg-accent animate-pulse" />
        )}
      </span>
      <span
        className="text-[10px] text-text-subtle"
        data-test-id={state === "unknown" ? "provider-peak-status" : state === "blocked" ? "provider-peak-status-blocked" : "provider-peak-status-open"}
        title={title}
      >
        {unknown ? (
          <>
            unknown
            <button
              type="button"
              onClick={() => { setUnknown(true); refreshRef.current(); }}
              className="ml-1 underline hover:text-text"
              title="Retry peak-hours status"
            >
              retry
            </button>
          </>
        ) : paused
          ? `paused until ${fmt(status?.pausedUntil)}`
          : blocked
            ? `blocked · reopens ${fmt(status?.reopensAt)}`
            : `open${status?.nextTransition ? ` · next ${fmt(status.nextTransition)}` : ""}`}
        {!unknown && status?.timezone ? ` · ${status.timezone}` : ""}
        {!unknown && status?.checkedAt ? ` · checked ${fmt(status.checkedAt)}` : ""}
      </span>
    </>
  );
}
