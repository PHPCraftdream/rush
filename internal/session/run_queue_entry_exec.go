// Leased-entry execution: executeEntrySync drives a leased entry through
// Coordinator.Run under the lease-renewal loop and watchdog, then writes
// the matching Ack/Nack/TerminalFail outcome for the row it owns.

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// cancelCause identifies which of the two independent mechanisms inside
// executeEntrySync cancelled execCtx and set leaseLost: the deadline-based
// watchdog goroutine, or the renewal loop's own `!ok` branch (a
// RenewRunQueueLease call that reached the DB and learned the row was
// reassigned). See cancelCauseAtomic's doc at its declaration for why a
// test needs to distinguish the two rather than only observing that some
// cancellation occurred (task #611).
type cancelCause int32

const (
	// cancelCauseNone means neither mechanism cancelled this execution —
	// Coordinator.Run simply returned on its own.
	cancelCauseNone cancelCause = iota
	// cancelCauseWatchdog means the independent deadline-based watchdog
	// goroutine fired first.
	cancelCauseWatchdog
	// cancelCauseRenewalNotOK means the renewal loop's own `!ok` branch —
	// RenewRunQueueLease successfully reached the DB and learned the row
	// had already been reassigned to a different owner — fired first. This
	// is the branch P1-2 exists to cover.
	cancelCauseRenewalNotOK
)

// CancelCauseForTest is an exported alias of the package-private cancelCause,
// letting the external session_test package assert on the value reported by
// RunQueuePumpConfig.TestOnCancelCause without exposing cancelCause itself
// as part of the public API. Test-only seam (task #611).
type CancelCauseForTest = cancelCause

// Exported constants mirroring cancelCause's values, for use by the external
// session_test package. Test-only seam (task #611).
const (
	CancelCauseNoneForTest         = cancelCauseNone
	CancelCauseWatchdogForTest     = cancelCauseWatchdog
	CancelCauseRenewalNotOKForTest = cancelCauseRenewalNotOK
)

// executeEntrySync is executeEntry's body, extracted (task #421/P0-1) so a
// synchronous caller — DrainSessionNow — can invoke the exact same
// lease/renew/watchdog/ack machinery without going through the async
// worker-pool bookkeeping (workerWg/execSem/inFlight) that wraps every
// executeEntry call site; each caller manages that bookkeeping itself, in
// whatever shape fits its own concurrency model.
//
// Returns nil ONLY on an ack'd success — the turn ran and its row was
// committed (deleted). Nothing else may return nil.
//
// On failure, returns the underlying error (including the
// ErrCallQueuedNotExecuted sentinel, or an error matching
// AlreadyAttempted/*SessionLockBusyError) after already performing the
// matching Ack/Nack/TerminalFail write — callers never need to interpret
// the error to decide what to persist for the row, only what to do next on
// their own side (retry, stop, surface as a failure).
//
// Two outcomes have no row write behind them and must be distinguished from
// both success and an ordinary failure:
//
//   - errLeaseLost — the lease was reassigned mid-execution (see leaseLost's
//     own doc). The new owner writes the outcome; this attempt learns nothing.
//   - ErrTurnCommitFailed — the turn SUCCEEDED but the Ack did not, so the
//     row is still leased and will be recovered and re-run after its lease
//     expires. Callers must not re-run the provider on their own, and must
//     not report a terminal success. This used to return nil (P0-3 of the
//     2026-08-18 release-readiness review), which made `crush run` tell the
//     operator a durable continuation had completed when nothing had been
//     committed at all.
func (p *RunQueuePump) executeEntrySync(ctx context.Context, leased *RunQueueEntry) error {
	// newDBCtx creates a fresh, short-lived (30s) context for a single DB
	// write, deliberately rooted in context.Background() rather than p.ctx or
	// execCtx: it must outlive both the pump's own lifecycle (Ack/Nack/
	// TerminalFail must still be able to write after Stop() has canceled
	// p.ctx — the original P0-3 rationale) and, separately, the in-flight
	// Coordinator.Run call.
	//
	// A single dbCtx created once at function entry (the original P0-3
	// shape) does NOT work: its 30s deadline is measured from before
	// Coordinator.Run is ever called, but Run can legitimately take minutes
	// for a real turn. Reusing that same, already-expired context for the
	// post-Run outcome write (Ack/Nack/TerminalFail) made every such write
	// fail with "context deadline exceeded" for any turn over 30s, leaving
	// the row leased/pending and causing the exact duplicate-execution bug
	// P0-3 exists to prevent — found during the closing review of this
	// round. Each DB write below now gets its own fresh 30s budget instead.
	newDBCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), p.dbWriteTimeout())
	}

	// The execution context is derived from the CALLER's ctx, not Background.
	//
	// It still allows cancelling Coordinator.Run when this executor loses
	// lease ownership during the renewal loop (P1-2) — that is what the
	// explicit cancel below is for. What changed is the parent.
	//
	// Rooting it in Background made the ctx parameter decorative: this
	// function compiled with ctx entirely unused, so a caller's timeout or
	// cancellation could not stop a continuation that had already begun.
	// The consequences were not theoretical (P0-2, 2026-08-18
	// release-readiness review): `rush run --timeout` did not bound a
	// durable continuation, App.Shutdown could see an idle pump and close
	// the DB underneath a live execution, and the goroutine could keep
	// writing messages after the CLI had already returned an error to the
	// operator. That is the "the command finished but the session is still
	// alive" symptom.
	//
	// Callers must pass an execution-scoped parent: the pump passes its
	// long-lived p.ctx, DrainSessionNow passes its caller's ctx. A scan
	// context must never be passed here.
	//
	// The outcome writes below deliberately do NOT inherit this — see
	// newDBCtx above. Ack/Nack/TerminalFail must still land after the
	// execution context is cancelled, which is exactly when they matter most.
	execCtx, execCancel := context.WithCancel(ctx)
	defer execCancel()

	// leaseLost tracks whether this execution lost its lease ownership during
	// the renewal loop. When true, we skip all outcome writes (Ack/Nack/TerminalFail)
	// since the row no longer belongs to this executor — the new owner will
	// handle the outcome. This is a read-after-write flag: the renewal goroutine
	// sets it atomically, and the main execution reads it after Coordinator.Run.
	var leaseLost atomic.Bool

	// cancelCauseAtomic records WHICH of the two independent mechanisms that
	// can set leaseLost/execCancel actually fired first: the watchdog
	// goroutine (deadline-based, fires even if the DB is unreachable) or the
	// renewal loop's own `!ok` branch (RenewRunQueueLease succeeded in
	// reaching the DB and learned the row had already been reassigned).
	// Exactly one of the two CompareAndSwaps below can win per execution —
	// both goroutines call execCancel()/leaseLost.Store(true) unconditionally
	// on their own path, but only the first to reach its own
	// cancelCauseAtomic.CompareAndSwap(0, ...) gets to record itself as the
	// cause; the second writer's CAS is a no-op.
	//
	// Exists (task #611) because the two mechanisms produce the exact same
	// externally-visible effect — execCtx is cancelled and no outcome write
	// happens — so a test that only observes that effect cannot tell whether
	// it is exercising the `!ok` branch (the one P1-2 was actually written
	// for) or merely benefiting from the watchdog firing first for an
	// unrelated reason (e.g. a short TestLeaseTTL making the watchdog's own
	// deadline arrive before the stolen-lease renewal tick does). See
	// CancelCauseForTest's own doc for how a test reads this value.
	var cancelCauseAtomic atomic.Int32

	// Parse the call data from JSON
	var callData SessionAgentCallData
	if err := json.Unmarshal([]byte(leased.CallData), &callData); err != nil {
		slog.Error("run_queue_pump: failed to parse call data", "id", leased.ID, "session_id", leased.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		// Terminal failure: malformed data can never succeed
		parseDBCtx, parseDBCancel := newDBCtx()
		defer parseDBCancel()
		if termErr := p.cfg.Sessions.TerminalFailRunQueueEntry(parseDBCtx, leased.ID, p.cfg.PumpInstanceID); termErr != nil {
			slog.Error("run_queue_pump: terminal fail failed", "id", leased.ID, "err", termErr, "instance_id", p.cfg.PumpInstanceID)
		}
		return err
	}

	// Renew this lease periodically while Coordinator.Run is in flight.
	// renewCtx is rooted in context.Background(), NOT newDBCtx()'s 30s
	// budget: it must stay alive for the entire duration of Coordinator.Run,
	// which can legitimately run far longer than 30s, and is only ever
	// cancelled by stopRenewing() once Run returns (or by execCtx-driven
	// lease loss below). Each individual RenewRunQueueLease call still gets
	// its own fresh, short-lived DB context via newDBCtx() — a single
	// long-lived context would eventually starve a single slow DB call from
	// ever timing out, and a single short-lived one reused across ticks
	// would (as found in the closing review of this round) expire well
	// before the loop's own natural lifetime ends.
	//
	// Found by the fourth @oh review pass over #337-349: RunQueueLeaseTTL
	// (30s) is far shorter than a real LLM turn, and without renewal
	// CleanupExpiredLeases would return this entry to pending — status
	// 'pending', not 'leased' — while this goroutine is still genuinely
	// executing it. The eventual AckRunQueueEntry (`WHERE status =
	// leased`) would then silently fail to match, leaving the row pending
	// with an incremented attempts count, and the NEXT tick would lease
	// and dispatch a second, genuinely duplicate execution of the exact
	// same turn — the same data-loss/duplicate-execution outcome the
	// inFlight guard above was meant to close, just via a sequential path
	// instead of a concurrent one. Renewing well inside the TTL (every
	// TTL/3) keeps this pump's own lease alive for the entire duration of
	// a long turn under all but pathological scheduling delays (a >20s
	// stall between renewal ticks) — see the doc below on what happens if
	// renewal itself ever loses the race.
	//
	// P1-1 watchdog fix: The original fail-closed timeout (tracking
	// lastSuccessfulRenewal and checking time.Since(lastSuccessfulRenewal) >= TTL)
	// had a critical flaw: if a RenewRunQueueLease call started at ~10s and
	// hung until its own 30s timeout, the executor would continue running until
	// ~40s even though the lease expired at ~30s. During those extra ~10s, another
	// pump instance could legitimately take ownership and start a duplicate execution.
	//
	// The fix: introduce an INDEPENDENT watchdog timer (separate from the renewal
	// loop) that cancels execCtx BEFORE lease expiry with a safety margin (production:
	// 5s). The watchdog fires at TTL - safety_margin from the last successful renewal,
	// regardless of whether the next renewal call is stuck or not. This provides a
	// strong bound against double-execution even when the DB layer hangs.
	//
	// The watchdog is the sole lease-expiry enforcement mechanism. The old
	// fail-closed timeout (timeSinceLastSuccess >= TTL) has been removed because
	// the watchdog ALWAYS fires first at TTL - safety_margin (where safety_margin > 0
	// in production and test configs). The watchdog provides a stricter guarantee
	// (TTL - margin vs TTL), eliminating the double-execution window the old code
	// could not catch when DB writes stalled.
	//
	// Additionally, each RenewRunQueueLease call now gets a timeout equal to the
	// remaining safe lease budget (time until watchdog would fire), not a fixed 30s.
	// This ensures a stalled renewal cannot outlive the safe window.
	//
	// P0-3 fix: P1-1's watchdog still measured "TTL - margin from the last
	// successful renewal", where "last successful renewal" meant the wall-clock
	// time the RenewRunQueueLease call RETURNED (post-call), while the
	// lease_expires_at value it wrote to the DB was computed from the
	// wall-clock time the call STARTED (pre-call), via
	// newExpiresAt := time.Now().Add(TTL) before the DB round-trip. For a
	// FAST renewal these are indistinguishable, but for a SLOW-but-successful
	// renewal (DB contention, GC pause, disk stall) that takes duration D to
	// complete, the watchdog would believe the lease is D newer than what is
	// actually recorded in the DB. If D exceeds the safety margin, the
	// watchdog fires AFTER the real DB expiry has already passed — during
	// that gap, another pump's CleanupExpiredLeases can reclaim the row and
	// dispatch a duplicate execution while this one is still believed
	// (locally) to be safely within its lease. The fix: track the ABSOLUTE
	// lease_expires_at (watchdogDeadlineAtomic below), seeded from the row's
	// initial lease_expires_at (as returned by LeaseRunQueueEntry) and
	// updated, on each successful renewal, to the exact newExpiresAt value
	// that was just written to the DB — never to a post-call time.Now(). The
	// watchdog then compares wall-clock now() directly against that absolute
	// deadline minus the safety margin, so its decision always matches what
	// is actually persisted, regardless of how long any individual renewal
	// call took.
	//
	// SEMANTICS: The durable queue provides AT-LEAST-ONCE guarantees for
	// persistent side effects (LLM calls, tool execution, message writes),
	// not exactly-once. The watchdog MINIMIZES but does NOT GUARANTEE
	// elimination of all duplicate-execution windows. The residual overlap window is:
	//   - From "watchdog fires and execCancel() propagates to Coordinator.Run"
	//     until "provider/tool actually stops respecting cancellation"
	//     (in-flight HTTP requests may complete).
	//   - A full fencing token (checked before each persistent write) is
	//     required for strict exactly-once semantics, which is an
	//     architectural change beyond this watchdog fix.
	//
	// Residual windows are bounded to the safety margin (production: 5s) plus
	// provider/tool cancellation latency, not the full TTL.
	renewCtx, stopRenewing := context.WithCancel(context.Background())
	renewalsDone := make(chan struct{})

	// P1-1 watchdog: an independent goroutine that cancels execCtx BEFORE
	// lease expiry if renewal stalls.
	//
	// P0-3 fix: the watchdog must compare against the ABSOLUTE
	// lease_expires_at value actually committed to the DB, not against
	// "time since the last successful renewal call returned". The two are
	// NOT interchangeable: newExpiresAt is computed as time.Now().Add(TTL)
	// BEFORE the RenewRunQueueLease DB call is issued, and that exact value
	// (not a commit-time value) is what gets written to lease_expires_at.
	// If a successful renewal call takes D to complete (slow DB, GC pause,
	// lock contention), then "time since post-call timestamp" understates
	// the row's actual age by D. A watchdog keyed off the post-call
	// timestamp would then keep the execution alive for up to D longer than
	// the row is actually leased for — if D exceeds the safety margin, the
	// watchdog fires AFTER the real DB expiry, leaving a window where
	// another pump's CleanupExpiredLeases can already have reclaimed the
	// row and dispatched a duplicate execution while this one is still
	// running. Tracking the absolute expiry (seeded from the initially
	// leased row, then updated to the exact value written by each
	// successful renewal) makes the watchdog's deadline match the DB's
	// deadline exactly, independent of how long any individual renewal
	// call took.
	//
	// Design: Use atomic storage for the absolute lease deadline + ticker.
	// This avoids timer.Reset() complexity and channel race conditions.
	// The watchdog wakes every 10ms to check if we're past the safe deadline.
	//
	// The INITIAL seed uses leased.LeaseExpiresAt — the value
	// LeaseRunQueueEntry actually wrote to the DB. This is safe/precise
	// because LeaseRunQueueEntry rounds UP (ceiling) to the next whole Unix
	// second rather than flooring (see session.go's ceilUnixSeconds and its
	// use in LeaseRunQueueEntry): the persisted deadline can only be later
	// than the true, sub-second-precision intended deadline, by less than
	// 1s, never earlier. A plain floor here (or in LeaseRunQueueEntry) would
	// be fine for the production TTL (30s) but catastrophically imprecise
	// for the short TTLs (100s of milliseconds) this pump's own test suite
	// relies on to run quickly — e.g. a 300ms TTL would floor to +0 seconds,
	// seeding the watchdog with a deadline that has already passed. The
	// initial seed therefore still carries up to ~1s of the same one-time
	// rounding slack described below — unavoidable without also threading
	// LeaseRunQueueEntry's pre-rounding value back through RunQueueEntry,
	// which is out of scope here (this is a one-time cost paid once per
	// execution, not the per-renewal, potentially-repeated cost the fix
	// below targets).
	//
	// watchdogDeadlineAtomic tracks the deadline the watchdog's own firing
	// decision and the renewal loop's DB-timeout budget are measured
	// against. After the first successful renewal (see below), this holds
	// the TRUE, sub-second-precision intended deadline
	// (time.Now().Add(TTL), computed BEFORE ceilUnixSeconds rounds it up
	// for the whole-Unix-seconds DB column) — NOT the DB-persisted,
	// ceiling-rounded value that column actually holds.
	//
	// Found by direct measurement (task #604): ceilUnixSeconds's up-to-1s
	// rounding — deliberately one-directional (only ever later than the
	// true deadline, see ceilUnixSeconds's own doc, and REQUIRED to stay
	// that way so CleanupExpiredLeases never reclaims a row before the
	// watchdog would have cancelled it) — was silently consumed FROM the
	// watchdog's own safety margin every time this atomic was previously
	// seeded/updated from the rounded DB value: instrumented runs on a real
	// (WSL2/9p) Linux box showed the watchdog firing 97ms-930ms later than
	// TTL-margin predicts, purely from the arbitrary wall-clock phase a
	// renewal tick happened to land on — no scheduling delay involved (the
	// watchdog's own 10ms poll loop added only 1-9ms of slack in the same
	// runs). At TTL=10s/margin=2.5s (the flaky test's config) that is up to
	// 37% of the entire margin gone before any real jitter, which is what
	// turned ordinary Linux scheduling noise into an intermittent ~40%
	// failure rate (TestP1_1_WatchdogCancelsAtTTLMinusMargin, task #604).
	//
	// Using the true (pre-ceiling, so always <= the persisted value)
	// deadline for the post-first-renewal updates is strictly safer, never
	// weaker: the watchdog now fires at-or-before the time it used to fire
	// at, so it is still always well before the persisted lease_expires_at
	// column (which is what CleanupExpiredLeases and any other pump's
	// reclaim decision actually reads — this atomic is never consulted by
	// anything outside this function). The DB column itself, and the
	// ceiling rounding that produces it, are UNCHANGED — this only fixes
	// what the in-memory watchdog measures itself against.
	var watchdogDeadlineAtomic atomic.Int64 // Unix nanoseconds, watchdog's own deadline reference
	watchdogDeadlineAtomic.Store(time.Unix(leased.LeaseExpiresAt, 0).UnixNano())

	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-renewCtx.Done():
				// Renewal loop stopped
				return
			case <-ticker.C:
				// Check if watchdog should fire: compare now against the
				// deadline tracked in watchdogDeadlineAtomic — after the
				// first successful renewal, that is the TRUE (unrounded)
				// intended deadline, not the DB-persisted, ceiling-rounded
				// value the lease_expires_at column actually holds. See
				// watchdogDeadlineAtomic's doc above for why using the true
				// deadline here is strictly safer, not weaker.
				trueDeadline := time.Unix(0, watchdogDeadlineAtomic.Load())
				deadline := trueDeadline.Add(-p.leaseWatchdogSafetyMargin())

				if !time.Now().Before(deadline) {
					// Watchdog deadline passed: cancel execution
					leaseLost.Store(true)
					// task #611: record the watchdog as the cause, but only
					// if the `!ok` renewal branch has not already claimed
					// it — see cancelCauseAtomic's own doc above.
					cancelCauseAtomic.CompareAndSwap(int32(cancelCauseNone), int32(cancelCauseWatchdog))
					execCancel()
					slog.Error("run_queue_pump: lease watchdog fired: canceling execution before expiry",
						"id", leased.ID,
						"session_id", leased.SessionID,
						"ttl", p.leaseTTL(),
						"safety_margin", p.leaseWatchdogSafetyMargin(),
						"lease_expires_at", trueDeadline,
						"instance_id", p.cfg.PumpInstanceID)
					return
				}
			}
		}
	}()

	go func() {
		defer close(renewalsDone)
		// time.NewTicker panics on a non-positive interval — clamp so a
		// pathologically tiny TestLeaseTTL (a test foot-gun, never a
		// production value) can't crash the pump instead of just renewing
		// unnecessarily often.
		renewInterval := p.leaseTTL() / 3
		if renewInterval <= 0 {
			renewInterval = time.Millisecond
		}
		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()

		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				// P0-3: derive the DB call's timeout budget, and whether a
				// renewal attempt is even worthwhile, from the SAME absolute
				// deadline the watchdog uses — not from a local
				// "time since we last heard back" clock. The two used to
				// diverge whenever a renewal call itself took a while to
				// complete (see the watchdog goroutine's doc above for the
				// exact failure mode this closes).
				//
				// task #604: after the first successful renewal, this reads
				// the TRUE, unrounded deadline rather than the DB-persisted,
				// ceiling-rounded one — see watchdogDeadlineAtomic's doc
				// above. Sizing the DB call's timeout off the true deadline
				// only ever gives it an equal or SMALLER budget than before
				// (since the true deadline is <= the rounded one), so this
				// cannot make a renewal call outlive the watchdog's own
				// decision to fire.
				currentTrueExpiresAt := time.Unix(0, watchdogDeadlineAtomic.Load())
				timeUntilWatchdog := time.Until(currentTrueExpiresAt.Add(-p.leaseWatchdogSafetyMargin()))
				if timeUntilWatchdog <= 0 {
					// We're already past the safe budget: watchdog is imminent.
					// Don't even attempt renewal — let the watchdog fire.
					continue
				}

				// trueNewExpiresAt is the sub-second-precision intended
				// deadline, computed from time.Now() here, BEFORE the DB call
				// AND before ceiling-rounding. newExpiresAt (below) rounds
				// this UP to the next whole Unix second for the DB column —
				// that rounded value is what RenewRunQueueLease actually
				// writes to lease_expires_at on success (see
				// sql/run_queue.sql: RenewRunQueueLease sets lease_expires_at
				// = ? directly from the parameter).
				//
				// trueNewExpiresAt is what seeds watchdogDeadlineAtomic
				// (task #604): using the unrounded value for the watchdog's
				// OWN firing decision closes an up-to-1s gap that was
				// silently eaten out of the safety margin by ceilUnixSeconds
				// every time a renewal succeeded — see watchdogDeadlineAtomic's
				// doc for the measured magnitude (97ms-930ms observed on a
				// real Linux box, up to 37% of a 2.5s test margin) and why
				// using the true, always-earlier-or-equal deadline here is
				// strictly safer than the rounded one, never weaker.
				//
				// ceilUnixSeconds (not a plain .Unix() floor) for newExpiresAt:
				// lease_expires_at is a whole-Unix-seconds column, and a plain
				// floor can lose up to ~1s depending purely on the arbitrary
				// sub-second wall-clock phase a renewal happens to land on —
				// for short TTLs with correspondingly small safety margins
				// (this pump's own test suite uses TTLs as low as 100s of
				// milliseconds), that loss can exceed the ENTIRE margin,
				// making a lease appear already-expired to CleanupExpiredLeases
				// the instant it's renewed. Rounding UP instead is the safe
				// direction for the DB column: it can only make the persisted
				// deadline later than the true deadline, by less than 1s,
				// never earlier — see ceilUnixSeconds's doc and the P0-3 fix
				// note on LeaseRunQueueEntry for the matching initial-lease
				// case. That same up-to-1s-later property is exactly why the
				// DB column's value is no longer also used for the watchdog's
				// own timing above.
				trueNewExpiresAt := time.Now().Add(p.leaseTTL())
				newExpiresAt := ceilUnixSeconds(trueNewExpiresAt)
				renewDBCtx, renewDBCancel := context.WithTimeout(context.Background(), timeUntilWatchdog)
				ok, err := p.cfg.Sessions.RenewRunQueueLease(renewDBCtx, leased.ID, p.cfg.PumpInstanceID, newExpiresAt)
				renewDBCancel()
				if err != nil {
					slog.Warn("run_queue_pump: lease renewal failed, will retry next interval", "id", leased.ID, "session_id", leased.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
					continue
				}
				// P0-3 (task #604 update): update the watchdog's deadline
				// atomic to trueNewExpiresAt — the sub-second-precision
				// value computed BEFORE this same DB call and BEFORE
				// ceiling-rounding — NOT to time.Now() + TTL measured after
				// the call returned, and NOT to newExpiresAt (the rounded
				// value actually written to the DB column). Using post-call
				// time here would reintroduce the original P0-3 bug: a
				// renewal that took D to complete would make this atomic
				// believe the lease is D newer than it actually is in the
				// DB. Using the rounded value would reintroduce the task
				// #604 bug this atomic exists to close: see its doc above.
				watchdogDeadlineAtomic.Store(trueNewExpiresAt.UnixNano())

				// task #611: report what was just stored, plus both
				// candidate deadlines, so a test can assert the stored
				// value IS the true (pre-rounding) deadline and IS NOT
				// the rounded one — a direct value comparison, not an
				// inference from elapsed wall-clock time. See
				// TestOnWatchdogDeadlineStored's own doc for why this
				// closes a gap a broad timing window could not.
				if p.cfg.TestOnWatchdogDeadlineStored != nil {
					p.cfg.TestOnWatchdogDeadlineStored(
						time.Unix(0, watchdogDeadlineAtomic.Load()),
						trueNewExpiresAt,
						time.Unix(newExpiresAt, 0),
					)
				}

				if !ok {
					// The lease was already reassigned to a different
					// owner — this execution has lost the race and can no
					// longer safely keep the lease alive. Cancel execCtx
					// immediately to stop the in-flight Coordinator.Run
					// (P1-2). The eventual Ack/Nack/TerminalFail below
					// will then correctly fail to match (logged, not
					// silently treated as success) since those queries
					// are scoped to `leased_by = PumpInstanceID` (found by
					// the fifth @oh review pass — this WAS a real gap
					// before that scoping existed) and the row now
					// belongs to whatever re-leased it. We set leaseLost
					// so the main execution path knows to skip the outcome
					// write entirely and log a clear "aborted due to lease
					// loss" message instead of a confusing "0 rows matched"
					// error.
					leaseLost.Store(true)
					// task #611: record the `!ok` renewal branch as the
					// cause, but only if the watchdog has not already
					// claimed it — see cancelCauseAtomic's own doc above.
					// This is the branch P1-2 was actually written for; a
					// test asserting this specific value (not just that
					// SOME cancellation happened) is the only way to prove
					// this branch — rather than the watchdog racing ahead
					// of it — is what performed the cancellation.
					cancelCauseAtomic.CompareAndSwap(int32(cancelCauseNone), int32(cancelCauseRenewalNotOK))
					execCancel()
					slog.Error("run_queue_pump: lost lease ownership during renewal, canceling in-flight execution", "id", leased.ID, "session_id", leased.SessionID, "instance_id", p.cfg.PumpInstanceID)
					return
				}
			}
		}
	}()

	// Attempt to execute via coordinator.Run
	_, err := p.cfg.Coordinator.Run(execCtx, callData)

	// Stop renewing IMMEDIATELY once Run returns, synchronously, before any
	// of the outcome-handling below touches the row's lease (Ack/Nack/
	// RenewRunQueueLease-for-backoff). Deliberately not a `defer` — a
	// deferred stop would only run at function exit, leaving a window
	// where the renewal goroutine is still alive (and can still fire one
	// more tick) WHILE this function is already deciding the row's fate.
	// Observed directly: with a defer, the ErrCallQueuedNotExecuted branch
	// below could Nack the row back to pending, and the still-live renewal
	// goroutine could then fire and find `status != leased`, logging a
	// spurious "lost lease ownership" — misleading, since nothing else
	// actually took it; this execution's own Nack did.
	//
	// P1-1: also stop the watchdog goroutine.
	stopRenewing()
	<-renewalsDone
	<-watchdogDone

	// task #611: both goroutines have now fully joined, so cancelCauseAtomic
	// (if either ever set it) is stable — report it to any test that wants
	// to distinguish which mechanism performed a cancellation, rather than
	// only observing that cancellation happened. cancelCauseNone means
	// neither mechanism fired (the common case: the turn simply finished).
	if p.cfg.TestOnCancelCause != nil {
		p.cfg.TestOnCancelCause(cancelCause(cancelCauseAtomic.Load()))
	}

	// P1-2: If lease ownership was lost during the renewal loop, skip all
	// outcome writes. The row no longer belongs to this executor (the new
	// owner will handle Ack/Nack/TerminalFail), and attempting to write
	// would fail with "0 rows matched" and produce confusing log messages.
	// The leaseLost flag is set atomically by the renewal goroutine when
	// RenewRunQueueLease returns !ok, and execCancel() is called there
	// to stop the in-flight Coordinator.Run as early as possible.
	if leaseLost.Load() {
		slog.Debug("run_queue_pump: execution aborted due to lease loss, skipping outcome write (reconciliation deferred to new owner)", "id", leased.ID, "session_id", leased.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return errLeaseLost
	}

	// Fresh 30s budget for the outcome write below, created only now —
	// AFTER Coordinator.Run has already returned — not at function entry.
	// See newDBCtx's own doc for why reusing one context across the
	// entire Run() call was the bug.
	dbCtx, dbCancel := newDBCtx()
	defer dbCancel()

	// Handle outcome
	if err == nil {
		// Success: ack the entry (delete it). The "executed successfully" log
		// must be conditioned on the ack itself succeeding (P2-6) — logging it
		// unconditionally previously claimed success even when the row was
		// never actually deleted, which is misleading for anyone debugging a
		// row that keeps reappearing after an Ack failure.
		//
		// P0-3 of the 2026-08-18 release-readiness review: the ack failure was
		// logged and then `return nil` — this function's own contract reserves
		// nil for an ACKED success, so that was an outright false claim. The
		// row is still leased; its lease will expire; CleanupExpiredLeases will
		// return it to pending with an attempt charged; and the turn will run a
		// SECOND time. Meanwhile DrainSessionNow reported (true, nil) and the
		// CLI told the operator the work had completed.
		//
		// Returning ErrTurnCommitFailed does not prevent that second run —
		// only fencing or idempotent side effects could, and both are beyond
		// this function (see the SEMANTICS note on the watchdog above: the
		// durable queue is at-least-once by construction). What it does do is
		// stop the API from claiming a terminal commit that did not happen, so
		// the caller can report the truth instead of a success.
		//
		// Deliberately NOT nacked. A nack would put the row back to pending
		// immediately and re-run a turn whose side effects already landed. The
		// lease-expiry path takes RunQueueLeaseTTL to do the same thing, but it
		// charges an attempt each time, so a row whose ack is persistently
		// failing eventually dead-letters at RunQueueMaxAttempts rather than
		// re-running the same turn forever.
		//
		// A zero-rows ack (the row was re-leased by someone else between
		// Coordinator.Run returning and this write) is already an error, not a
		// silent no-op: AckRunQueueEntry maps sql.ErrNoRows to one — so it
		// travels this same path rather than being mistaken for a commit.
		if _, ackErr := p.cfg.Sessions.AckRunQueueEntry(dbCtx, leased.ID, p.cfg.PumpInstanceID); ackErr != nil {
			slog.Error("run_queue_pump: ack failed after success — the turn ran but was not committed, and may run again after the lease expires", "id", leased.ID, "session_id", leased.SessionID, "err", ackErr, "instance_id", p.cfg.PumpInstanceID)
			return fmt.Errorf("%w: %w", ErrTurnCommitFailed, ackErr)
		}
		slog.Info("run_queue_pump: executed entry successfully", "id", leased.ID, "session_id", leased.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return nil
	}

	// ErrCallQueuedNotExecuted means the call was appended to a genuinely
	// external live owner's mailbox (inFlight already rules out this being
	// the pump's own doing) rather than executed by this attempt — see that
	// error's doc for why it must be treated as neither success nor an
	// ordinary retryable failure.
	//
	// Found by the fourth @oh review pass over #337-349: the original fix
	// here left the entry exactly as leased and did nothing further,
	// relying on CleanupExpiredLeases's natural expiry as the sole
	// recovery path — but that same cleanup unconditionally increments
	// attempts on every recovery (needed elsewhere, see its own SQL
	// comment, to eventually dead-letter a poison entry that always
	// crashes before a normal Ack/Nack). Treated as an ordinary lease
	// expiry, a session that simply stays externally busy for
	// RunQueueMaxAttempts * RunQueueLeaseTTL (a few minutes) would have
	// its accepted, entirely healthy, never-actually-failed work silently
	// deleted — the exact class of bug SessionLockBusyError's
	// no-attempt-penalty handling exists to prevent for the equivalent
	// OS-lock case, applied inconsistently here for the in-process case.
	//
	// Fixed via NackRunQueueEntryNoAttemptPenalty (immediate release, no
	// attempts increment — mirroring SessionLockBusyError's own handling
	// below) plus a LOCAL busyBackoffUntil deadline recorded for this
	// session so THIS pump instance does not immediately re-lease and
	// re-dispatch the same entry into the same busy owner's mailbox on
	// the very next tick (mailbox.submit has no dedup — a tight retry
	// loop would append a new duplicate call on every attempt). See the
	// busyBackoffUntil field's own doc for why a single RenewRunQueueLease
	// call (tried first) did not actually work: it happens almost
	// instantly after the original lease, so it barely extends
	// lease_expires_at beyond what leasing already set, and
	// CleanupExpiredLeases still reaped the row — and charged an attempt
	// — after essentially one ordinary TTL window, same as doing nothing.
	if errors.Is(err, ErrCallQueuedNotExecuted) {
		if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(dbCtx, leased.ID, p.cfg.PumpInstanceID, "queued into an externally-owned in-process session, not executed"); nackErr != nil {
			slog.Error("run_queue_pump: no-penalty release after queued-not-executed failed", "id", leased.ID, "session_id", leased.SessionID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
		}
		p.busyBackoffMu.Lock()
		p.busyBackoffUntil[leased.SessionID] = time.Now().Add(p.leaseTTL())
		p.busyBackoffMu.Unlock()
		slog.Debug("run_queue_pump: call was queued into an externally-owned session, backed off locally without an attempt penalty", "id", leased.ID, "session_id", leased.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return err
	}

	// Failure: determine if it's retryable or terminal
	// Use the marker interface to detect terminal failures (task #339 protection)
	// without creating an import cycle between session and agent packages.
	var alreadyAttempted AlreadyAttempted
	if errors.As(err, &alreadyAttempted) && alreadyAttempted.AlreadyAttempted() {
		// Terminal failure (no retry) - protects against duplicates
		if termErr := p.cfg.Sessions.TerminalFailRunQueueEntry(dbCtx, leased.ID, p.cfg.PumpInstanceID); termErr != nil {
			slog.Error("run_queue_pump: terminal fail failed", "id", leased.ID, "err", termErr, "instance_id", p.cfg.PumpInstanceID)
		}
		slog.Warn("run_queue_pump: entry terminal failed (already attempted)", "id", leased.ID, "session_id", leased.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		return err
	}

	// SessionLockBusyError means another live process legitimately holds the
	// OS session lock right now — ordinary, expected contention (a normal
	// turn can hold it for as long as a full LLM turn takes), not a failure
	// of the call itself. It must never count toward RunQueueMaxAttempts:
	// counting it would let a few turns' worth of routine contention
	// silently delete accepted user work once attempts exhausts (found in
	// the final @oh review of tasks #337-349 — P0-2).
	var busyErr *SessionLockBusyError
	if errors.As(err, &busyErr) {
		if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(dbCtx, leased.ID, p.cfg.PumpInstanceID, err.Error()); nackErr != nil {
			slog.Error("run_queue_pump: no-penalty nack failed", "id", leased.ID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
		}
		slog.Debug("run_queue_pump: entry blocked by session lock contention, will retry without attempt penalty", "id", leased.ID, "session_id", leased.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return err
	}

	// Retryable failure: nack and let the pump retry on next tick
	if nackErr := p.cfg.Sessions.NackRunQueueEntry(dbCtx, leased.ID, p.cfg.PumpInstanceID, err.Error()); nackErr != nil {
		slog.Error("run_queue_pump: nack failed", "id", leased.ID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
	}
	slog.Debug("run_queue_pump: entry failed, will retry", "id", leased.ID, "session_id", leased.SessionID, "err", err, "attempts", leased.Attempts+1, "instance_id", p.cfg.PumpInstanceID)
	return err
}
