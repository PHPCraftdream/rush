import { AntiHookComponent, bind } from "@react-carburetor/core";
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

/**
 * Formats an RFC3339 timestamp in the browser's locale.
 *
 * @param ts - the timestamp, if any
 */
function fmt(ts?: string): string {
  return ts ? new Date(ts).toLocaleString() : "—";
}

/**
 * Parses a possibly-missing timestamp to unix milliseconds.
 *
 * @param t - the timestamp string
 */
function toMillis(t: string | undefined): number {
  return new Date(t as string).getTime();
}

/**
 * Whether a unix-millisecond time lies in the future.
 *
 * @param t - the time
 */
function isFuture(t: number): boolean {
  return !Number.isNaN(t) && t > Date.now();
}

/**
 * The status row of one provider, if present.
 *
 * @param statuses - the reply rows
 * @param providerId - the provider to find
 */
function findStatus(
  statuses: readonly ProviderPeakHoursStatus[],
  providerId: string,
): ProviderPeakHoursStatus | null {
  return statuses.find((s) => s.providerId === providerId) ?? null;
}

/**
 * Delay until ~1s after the earliest future boundary, or null when none is within the 24h cap.
 *
 * Date.now() only schedules the fetch, state stays server-owned.
 *
 * @param s - the latest status
 */
function boundaryDelay(s: ProviderPeakHoursStatus): number | null {
  const candidates = [s.reopensAt, s.pausedUntil, s.nextTransition].filter(Boolean).map(toMillis);
  const future = candidates.filter(isFuture);
  if (future.length === 0) return null;
  const diff = Math.min(...future) - Date.now() + 1000;
  return diff > BOUNDARY_CAP_MS ? null : diff;
}

/** Props of the peak-hours badge. */
interface IPeakHoursStatusProps {
  providerId: string;
  // False when the provider has no peakHours configured: render nothing so
  // the row keeps "no badge" semantics (specs assert badge count 0).
  configured?: boolean;
  // Compact wire summary from ProviderInfo.peakHours (e.g. "09:00–18:00"),
  // shown on the badge itself; detailed state lives on the status line.
  summary?: string;
}

/** Local state of the peak-hours badge. */
interface IPeakHoursStatusState {
  status: ProviderPeakHoursStatus | null;
  // "unknown" while the first fetch is in flight or after an error.
  unknown: boolean;
}

/** Peak-hours badge and status line of one provider, driven by the server's status replies. */
export class PeakHoursStatus extends AntiHookComponent<IPeakHoursStatusProps, IPeakHoursStatusState> {
  public state: IPeakHoursStatusState = { status: null, unknown: true };

  private boundaryTimer: ReturnType<typeof setTimeout> | null = null;

  private fallbackTimer: ReturnType<typeof setInterval> | null = null;

  // Generation of the current polling run; bumped on setup and teardown so a
  // late reply of a previous provider (or an unmounted badge) is dropped.
  private run = 0;

  /** Polls the provider's status; restarts when the provider changes. */
  protected useEffects(): void {
    super.useEffects();
    this.useEffect("poll", this.startPolling, [this.props.providerId]);
  }

  /** Starts a polling run; returns its teardown. */
  @bind
  protected startPolling(): () => void {
    this.run += 1;
    this.setState({ unknown: true, status: null });
    void this.refresh(this.run);
    // Single bounded fallback refetch while mounted.
    this.fallbackTimer = setInterval(this.onTick, FALLBACK_INTERVAL_MS);
    return this.stopPolling;
  }

  /** Ends the polling run: drops late replies and clears both timers. */
  @bind
  protected stopPolling(): void {
    this.run += 1;
    this.clearBoundary();
    if (this.fallbackTimer !== null) clearInterval(this.fallbackTimer);
    this.fallbackTimer = null;
  }

  /** Refetches on a timer tick. */
  @bind
  protected onTick(): void {
    void this.refresh(this.run);
  }

  /** Cancels the pending boundary refetch. */
  protected clearBoundary(): void {
    if (this.boundaryTimer !== null) clearTimeout(this.boundaryTimer);
    this.boundaryTimer = null;
  }

  /**
   * Schedules the boundary refetch ~1s after the earliest future boundary (≤24h).
   *
   * Updates the badge AT the transition instead of polling globally.
   *
   * @param s - the latest status
   */
  protected scheduleBoundary(s: ProviderPeakHoursStatus | null): void {
    this.clearBoundary();
    if (!s) return;
    const diff = boundaryDelay(s);
    if (diff === null) return;
    this.boundaryTimer = setTimeout(this.onTick, diff);
  }

  /**
   * Fetches the provider's status and applies it unless the run was superseded.
   *
   * @param run - the polling run this fetch belongs to
   */
  protected async refresh(run: number): Promise<void> {
    const { providerId } = this.props;
    try {
      const reply = await fetchPeakHoursStatus(providerId);
      if (run !== this.run) return;
      const mine = findStatus(reply.statuses, providerId);
      this.setState({ status: mine, unknown: false });
      this.scheduleBoundary(mine);
    } catch {
      if (run !== this.run) return;
      this.setState({ unknown: true });
    }
  }

  /** Marks the status unknown and refetches it. */
  @bind
  protected retry(): void {
    this.setState({ unknown: true });
    void this.refresh(this.run);
  }

  /** Renders the badge and the status line; nothing when peak hours are not configured. */
  public render() {
    const { configured, summary } = this.props;
    const { status, unknown } = this.state;
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
    const statusTestId =
      state === "unknown"
        ? "provider-peak-status"
        : state === "blocked"
          ? "provider-peak-status-blocked"
          : "provider-peak-status-open";

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
          data-test-id={statusTestId}
          title={title}
        >
          {unknown ? (
            <>
              unknown
              <button
                type="button"
                onClick={this.retry}
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
}
