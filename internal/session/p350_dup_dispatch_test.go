package session_test

// P350 regression tests (found across the third and fourth @oh review
// passes over #337-349, 2026-08-10): run_queue_pump.go's executeEntry
// treated Coordinator.Run's nil error as unconditional success and Acked
// (deleted) the durable row regardless of whether the call actually ran.
// agent.sessionAgent.Run returns (nil, nil) — not an error — when the
// target session is already owned by a live, in-process turn: the call is
// merely appended to that owner's mailbox queue (mailbox.submit
// unconditionally appends, no dedup), not executed by this call.
//
// This was reachable three ways:
//  1. Same-tick (third pass): tick() leases and `go executeEntry`-dispatches
//     entries one at a time in a single pass; two distinct durably-queued
//     entries for the same session (e.g. two calls queued while a process
//     was down) could be leased and dispatched back to back within the
//     same tick, before either had run long enough to matter.
//  2. Sequential self-inflicted (found by the third pass, actually fixed by
//     the fourth): RunQueueLeaseTTL (30s) is far shorter than a real LLM
//     turn can take. The third pass's inFlight guard (below) does NOT
//     close this path on its own — inFlight only blocks a SECOND
//     concurrent dispatch while the first goroutine is still tracked as
//     busy; it does nothing about the underlying DB row silently flipping
//     from 'leased' to 'pending' out from under a still-running execution.
//     Without lease renewal, CleanupExpiredLeases would do exactly that
//     after 30s, and the ORIGINAL goroutine's own eventual AckRunQueueEntry
//     (`WHERE status = leased`) would then silently fail to match (logged,
//     not delete anything), leaving the row pending for a LATER tick to
//     lease and dispatch — a genuine duplicate execution of the same turn,
//     sequential rather than concurrent, but exactly as harmful.
//  3. External busy-then-recovered (found by the fourth pass): the original
//     fix for ErrCallQueuedNotExecuted (see below) left the entry exactly
//     as leased and untouched, relying solely on CleanupExpiredLeases's
//     natural lease-expiry as its recovery path — but that same cleanup
//     unconditionally increments attempts on every recovery. A session
//     that stayed externally busy for RunQueueMaxAttempts lease windows (a
//     few minutes) would have its accepted, never-actually-failed work
//     silently dead-lettered (deleted) — the same class of bug
//     SessionLockBusyError's no-attempt-penalty handling exists to prevent
//     for the equivalent cross-process OS-lock case.
//
// Fixed:
//  1. RunQueuePump.inFlight tracks session IDs with an executeEntry
//     goroutine currently running FROM THIS PUMP INSTANCE; processEntry
//     refuses to lease a pending entry for a session already in that set.
//     Closes path 1 above at the source — the pump itself can never
//     concurrently dispatch two entries for one session.
//  2. executeEntry now runs a renewal loop (ticker at leaseTTL/3) alongside
//     Coordinator.Run, calling the new RenewRunQueueLease query to push
//     lease_expires_at forward — scoped to `status = leased AND leased_by
//     = ?` so a lease already reassigned to a different owner is never
//     silently extended out from under it. Closes path 2: a still-running
//     execution's lease can no longer expire underneath it under normal
//     scheduling.
//  3. session.ErrCallQueuedNotExecuted is returned by
//     coordinatorAdapterImpl.Run when the underlying call returns (nil,
//     nil) — i.e. queued into a genuinely EXTERNAL live owner the inFlight
//     guard has no visibility into. executeEntry treats this specially: no
//     Ack (the row would be deleted for work that has not actually run),
//     and — closing path 3 — an immediate
//     NackRunQueueEntryNoAttemptPenalty release (never counts an attempt,
//     mirroring SessionLockBusyError's own handling) paired with a LOCAL
//     RunQueuePump.busyBackoffUntil deadline so THIS pump instance does not
//     immediately re-lease and re-dispatch the same entry on the very next
//     tick — mailbox.submit appends unconditionally on every call, so an
//     uncontrolled retry loop would append a new duplicate on every
//     attempt. A single RenewRunQueueLease call was tried first and did NOT
//     work: it happens almost instantly after the original lease, barely
//     extending lease_expires_at beyond what leasing already set, so
//     CleanupExpiredLeases still reaped the row (and charged an attempt)
//     after essentially one ordinary TTL window — same as doing nothing.
//
// RunQueueLeaseTTL itself (30s) is not test-overridable, which made path 2
// impossible to exercise deterministically and quickly with a real timed
// test — RunQueuePumpConfig.TestLeaseTTL (mirroring the existing TestTick
// seam) was added to close that gap; see
// TestReleaseGate_P350_LeaseRenewedDuringLongExecution and
// TestReleaseGate_P350_QueuedNotExecutedBacksOffWithoutAttemptPenalty below.
// The time-dependent tests here run on RunQueuePumpConfig.TestClock (a fake
// clock, see pump_fake_clock_test.go) instead of real sleeps.

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// concurrencyTrackingCoordinator blocks every call on a shared gate (open
// by default) and tracks, per session ID, whether more than one call is
// ever in flight at the same time — the exact condition the inFlight guard
// must prevent.
type concurrencyTrackingCoordinator struct {
	mu             sync.Mutex
	activePerSess  map[string]int
	sawConcurrency bool

	gateMu sync.Mutex
	closed bool
	gate   chan struct{}

	calls atomic.Int64
}

func newConcurrencyTrackingCoordinator() *concurrencyTrackingCoordinator {
	return &concurrencyTrackingCoordinator{
		activePerSess: make(map[string]int),
		gate:          make(chan struct{}),
	}
}

// hold blocks all in-flight (and future) calls until release is called.
func (c *concurrencyTrackingCoordinator) hold() {
	c.gateMu.Lock()
	defer c.gateMu.Unlock()
	c.closed = true
}

func (c *concurrencyTrackingCoordinator) release() {
	c.gateMu.Lock()
	defer c.gateMu.Unlock()
	if c.closed {
		close(c.gate)
		c.closed = false
	}
}

func (c *concurrencyTrackingCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)

	c.mu.Lock()
	c.activePerSess[callData.SessionID]++
	if c.activePerSess[callData.SessionID] > 1 {
		c.sawConcurrency = true
	}
	c.mu.Unlock()

	// Block here if the gate is currently held, so the test can force an
	// overlap window that would expose a missing inFlight guard.
	c.gateMu.Lock()
	gate := c.gate
	closed := c.closed
	c.gateMu.Unlock()
	if closed {
		<-gate
	}

	c.mu.Lock()
	c.activePerSess[callData.SessionID]--
	c.mu.Unlock()

	var result any = "ok"
	return &result, nil
}

// TestReleaseGate_P350_NoDuplicateDispatchForSameSession proves that two
// distinct durably-queued entries for the SAME session are never dispatched
// concurrently by one pump instance — the same-tick path to the bug
// described in this file's top comment.
//
// NO EXTERNAL POKE: only a real RunQueuePump (TestTick for speed) drives
// execution; the test observes outcomes through the coordinator's own
// concurrency tracking and the durable queue's state.
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_pump.go's processEntry, remove the inFlight busy-check
//     block (the one returning early when the session is already busy).
//  2. Run: go test -run TestReleaseGate_P350_NoDuplicateDispatchForSameSession -v -race -count=5
//  3. FAIL (or race-detected, depending on timing): sawConcurrency becomes
//     true — both entries' coordinator.Run calls overlap.
//  4. Restore the inFlight guard and PASS.
func TestReleaseGate_P350_NoDuplicateDispatchForSameSession(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "test-session-dup-dispatch")
	ctx := t.Context()

	mkCallData := func(prompt string) []byte {
		callData := map[string]any{"SessionID": sess.ID, "Prompt": prompt}
		callDataJSON, err := json.Marshal(callData)
		require.NoError(t, err)
		return callDataJSON
	}

	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "dup-dispatch-probe-1", sess.ID, mkCallData("first queued call")))
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "dup-dispatch-probe-2", sess.ID, mkCallData("second queued call")))

	coord := newConcurrencyTrackingCoordinator()
	coord.hold() // force any overlapping dispatch to actually overlap, not race past by luck

	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "dup-dispatch-pump",
		TestTick:       func() time.Duration { return 20 * time.Millisecond },
	})
	pump.Start()
	defer pump.Stop()

	// Let the first call start and several more ticks elapse while it is
	// held — if the inFlight guard is missing, the second entry gets
	// leased and dispatched during this window, overlapping the first.
	require.Eventually(t, func() bool {
		return coord.calls.Load() >= 1
	}, 2*time.Second, 10*time.Millisecond, "first call must start")
	time.Sleep(300 * time.Millisecond) // ~15 ticks at 20ms

	coord.mu.Lock()
	callsWhileHeld := coord.calls.Load()
	sawConcurrencyMidHold := coord.sawConcurrency
	coord.mu.Unlock()
	require.Equal(t, int64(1), callsWhileHeld, "only ONE of the two entries should have been dispatched while the first is still in flight — the second must wait for inFlight to clear")
	require.False(t, sawConcurrencyMidHold, "no two calls for the same session should ever run concurrently")

	coord.release()

	// Now both entries should complete in sequence.
	require.Eventually(t, func() bool {
		return coord.calls.Load() >= 2
	}, 2*time.Second, 10*time.Millisecond, "second call must eventually start once the first releases")

	// Wait for BOTH entries to be Acked (deleted) — not merely leased.
	//
	// A pending-only predicate ("len(pending) == 0") is satisfied the moment
	// the SECOND entry is leased, while its Run is still executing: a leased
	// row is invisible to ListPendingRunQueueEntries. The wait would then
	// return before the second run finished, and a hypothetical THIRD
	// dispatch — exactly the duplicate this test exists to catch — landing
	// after that snapshot would go unnoticed by the "start to finish" and
	// exactly-two-calls assertions below. AckRunQueueEntry runs only after
	// Run returns, so "gone from pending AND leased" (runQueueGoneEverywhere)
	// orders those assertions after every outcome write of both runs.
	require.Eventually(t, func() bool {
		gone, err := runQueueGoneEverywhere(ctx, svc)
		return err == nil && gone
	}, 2*time.Second, 10*time.Millisecond, "both entries must eventually be acked (deleted, not merely leased)")

	coord.mu.Lock()
	defer coord.mu.Unlock()
	require.False(t, coord.sawConcurrency, "no two calls for the same session must ever have run concurrently, start to finish")
	require.Equal(t, int64(2), coord.calls.Load(), "exactly two calls total — one per queued entry, no duplicates")
}

// queuedNotExecutedCoordinator always reports the call as queued into an
// external owner, never actually executing it.
type queuedNotExecutedCoordinator struct {
	calls atomic.Int64
}

func (c *queuedNotExecutedCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	return nil, session.ErrCallQueuedNotExecuted
}

// TestReleaseGate_P350_QueuedNotExecutedNeitherAcksNorSpamRetries proves
// that when Coordinator.Run reports ErrCallQueuedNotExecuted, executeEntry
// does not Ack (delete) the durable row — since the work has not actually
// run — and does not immediately retry it either (which would append a new
// duplicate to the external owner's mailbox on every tick): the row is
// released (visible as pending again, matching SessionLockBusyError's own
// no-penalty handling) but this pump instance's local busyBackoffUntil
// deadline prevents IT from re-leasing the same session again immediately.
//
// Runs on a fake clock (RunQueuePumpConfig.TestClock): the backoff window is
// a TTL of FAKE time, so "still inside the window" and "window elapsed" are
// exact, not a race between a real sleep and a real deadline.
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_entry_exec.go's executeEntry, remove the
//     `errors.Is(err, ErrCallQueuedNotExecuted)` branch (falls through to
//     the generic Nack path, which DOES increment attempts — a different,
//     already-covered regression) — or, to specifically target the
//     no-immediate-retry guarantee this test checks, remove just the
//     `p.busyBackoffUntil[...] = ...` line while keeping the
//     NackRunQueueEntryNoAttemptPenalty call.
//  2. Run: go test -run TestReleaseGate_P350_QueuedNotExecutedNeitherAcksNorSpamRetries -v
//  3. FAIL: calls grows past 1 inside the window (spam-retried) instead of
//     staying pinned at 1.
//  4. Restore the branch and PASS.
func TestReleaseGate_P350_QueuedNotExecutedNeitherAcksNorSpamRetries(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "test-session-queued-not-executed")
	ctx := t.Context()

	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, "queued-not-executed-probe", sess.ID, callDataJSON))

	coord := &queuedNotExecutedCoordinator{}

	// Fake TTL, so the pump's local backoff (one TTL) is an hour of fake time.
	const ttl = time.Hour
	clk := newFakePumpClock(fakePumpEpoch)
	probe := newFakeClockService(svc, clk)
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       probe,
		Coordinator:    coord,
		PumpInstanceID: "queued-not-executed-pump",
		TestTick:       func() time.Duration { return ttl / 10 },
		TestLeaseTTL:   ttl,
		TestClock:      clk,
	})
	pump.Start()
	t.Cleanup(func() { pump.Stop() })

	awaitPump(t, func() bool { return coord.calls.Load() >= 1 }, "the entry must be leased and attempted once")
	// Released back to pending before any fake time passes, so no cleanup
	// pass can see it leased.
	awaitPump(t, func() bool {
		e, getErr := svc.GetRunQueueEntry(ctx, "queued-not-executed-probe")
		return getErr == nil && e != nil && e.Status == "pending"
	}, "the entry must be released back to pending")

	// Tick 5 times inside the backoff window (0.5 TTL of fake time). Tick N+1
	// only starts after tick N returned, so once the 5th tick's cleanup has
	// run, ticks 1-4 (each of which would have re-dispatched a non-backed-off
	// entry) are complete.
	for i := int64(1); i <= 5; i++ {
		clk.Advance(ttl / 10)
		awaitPump(t, func() bool { return probe.cleanups.Load() >= 1+i }, "tick must run")
	}
	require.Equal(t, int64(1), coord.calls.Load(), "must not be retried while still within its local busy-backoff window — retrying would append a duplicate to the external owner's mailbox on every attempt")
	require.Equal(t, int64(1), probe.leases.Load(), "the backed-off entry must not even be re-leased inside the window")

	// Must not have been Acked (deleted) either: it should still exist,
	// durably, released back to pending (not leased forever, not gone).
	pending, err := svc.ListPendingRunQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the entry must still exist, released back to pending — not acked/deleted for work that never actually ran")
	require.Equal(t, sess.ID, pending[0].SessionID)
	require.Equal(t, int64(0), pending[0].Attempts, "must not have incurred an attempt penalty for external contention")

	// The backoff is bounded: once a full TTL of fake time has passed the
	// entry is attempted again.
	clk.Advance(ttl)
	awaitPump(t, func() bool { return coord.calls.Load() >= 2 }, "the entry must be retried once its backoff window elapsed")
}

// slowCoordinator blocks every call until release is closed, tracking how
// many times it has been entered. Simulates a real, long-running LLM turn.
type slowCoordinator struct {
	calls   atomic.Int64
	release chan struct{}
}

func (c *slowCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	<-c.release
	var result any = "ok"
	return &result, nil
}

// TestReleaseGate_P350_LeaseRenewedDuringLongExecution proves that a call
// held in flight across MANY lease TTL windows is executed exactly once —
// the lease-renewal loop must keep the entry's row genuinely 'leased' for
// the whole duration, so CleanupExpiredLeases never returns it to pending
// while it is still running, and no later tick dispatches a duplicate.
//
// Fully deterministic: the pump runs on a fake clock
// (RunQueuePumpConfig.TestClock) and the test advances it one renewal
// interval (TTL/3) at a time, waiting after each step for the pump to have
// renewed and cleaned up at that instant. Nothing depends on wall-clock
// scheduling. This replaces a version that slept ~3.5 real TTLs and failed
// under load: each renewal's DB call carries a real timeout of only
// (TTL - TTL/3 - watchdog margin), which a loaded machine could overrun
// ("lease renewal failed ... context deadline exceeded"), letting the
// lease lapse and a second dispatch through. Widening the TTL only moved
// that cliff. Here the TTL is a fake hour, so that real timeout is ~40
// minutes and cannot expire.
//
// What is asserted at every step, on the real DB row: still 'leased' by
// this pump with attempts 0 (CleanupExpiredLeases, run at the fake instant,
// did not reap it), and lease_expires_at moved to exactly now+TTL (the
// renewal actually extended it). Then, after the call completes: the row is
// acked and later ticks dispatch nothing more.
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_entry_exec.go's executeEntry, disable the renewal loop,
//     e.g. change `case <-ticker.C():` in the renewal goroutine to
//     `case <-(chan time.Time)(nil):` (never fires) or wrap the ticker-case
//     body in `if false {`.
//  2. Run: go test -run TestReleaseGate_P350_LeaseRenewedDuringLongExecution -v -count=5
//  3. FAIL at the first step: the lease is never renewed.
//  4. Restore the renewal loop and PASS.
func TestReleaseGate_P350_LeaseRenewedDuringLongExecution(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "test-session-lease-renewal")
	ctx := t.Context()

	const entryID = "lease-renewal-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "long running call"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, entryID, sess.ID, callDataJSON))

	coord := &slowCoordinator{release: make(chan struct{})}
	var releaseOnce sync.Once
	releaseCall := func() { releaseOnce.Do(func() { close(coord.release) }) }

	const (
		ttl        = time.Hour // fake time
		renewEvery = ttl / 3   // the pump's renewal interval
		windows    = 10        // TTL windows to hold the call across
		pumpID     = "lease-renewal-pump"
	)
	clk := newFakePumpClock(fakePumpEpoch)
	probe := newFakeClockService(svc, clk)
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       probe,
		Coordinator:    coord,
		PumpInstanceID: pumpID,
		TestTick:       func() time.Duration { return renewEvery },
		TestLeaseTTL:   ttl,
		TestClock:      clk,
	})
	pump.Start()
	t.Cleanup(func() { pump.Stop() })
	t.Cleanup(releaseCall) // runs before Stop, so Stop never waits on a held call

	awaitPump(t, func() bool { return coord.calls.Load() >= 1 }, "the entry must be leased and execution started")
	// Tick loop + the execution's watchdog and renewal tickers: all must
	// exist before time moves, or their first period would start late.
	awaitPump(t, func() bool { return clk.liveTickers() >= 3 }, "pump, watchdog and renewal tickers must be registered")

	for step := int64(1); step <= windows*3; step++ {
		clk.Advance(renewEvery)
		at := clk.Now()
		awaitPump(t, func() bool { return probe.renewals.Load() >= step },
			"the lease must be renewed at every renewal interval — the renewal loop is not running")
		awaitPump(t, func() bool { return probe.cleanups.Load() >= step+1 }, "the pump must clean up expired leases at every tick")

		require.Equal(t, at.Unix(), probe.lastCleanupBefore.Load(), "step %d: cleanup must run at the fake instant", step)
		entry, getErr := svc.GetRunQueueEntry(ctx, entryID)
		require.NoError(t, getErr)
		require.NotNil(t, entry, "step %d: row must still exist", step)
		require.Equal(t, "leased", entry.Status, "step %d: a renewed lease must never be returned to pending by CleanupExpiredLeases while the call is in flight", step)
		require.Equal(t, pumpID, entry.LeasedBy, "step %d", step)
		require.Equal(t, int64(0), entry.Attempts, "step %d: lease recovery charges an attempt; none may have happened", step)
		require.Equal(t, at.Add(ttl).Unix(), entry.LeaseExpiresAt, "step %d: renewal must have extended the lease to now+TTL", step)
		require.Equal(t, int64(1), coord.calls.Load(), "step %d: no second dispatch while the first call is in flight", step)
	}

	releaseCall()
	awaitPump(t, func() bool {
		entry, getErr := svc.GetRunQueueEntry(ctx, entryID)
		return getErr == nil && entry == nil
	}, "entry should be acked once the long call finally completes")

	// Later ticks must find nothing to dispatch: the row is durably gone,
	// not merely leased or bounced back to pending.
	for range 3 {
		before := probe.cleanups.Load()
		clk.Advance(renewEvery)
		awaitPump(t, func() bool { return probe.cleanups.Load() > before }, "tick must run")
		pending, listErr := svc.ListPendingRunQueueEntries(ctx)
		require.NoError(t, listErr)
		require.Empty(t, pending)
	}

	require.Equal(t, int64(1), coord.calls.Load(), "the long-running call must have been executed exactly once — renewal must have kept its lease alive across multiple TTL windows, preventing a duplicate dispatch")
}

// queuedNotExecutedThenSuccessCoordinator returns ErrCallQueuedNotExecuted
// for its first N calls, then succeeds — simulating a session that stays
// externally busy (owned by another live, in-process turn) for a while
// before freeing up.
type queuedNotExecutedThenSuccessCoordinator struct {
	busyUntilCall int64
	calls         atomic.Int64
}

func (c *queuedNotExecutedThenSuccessCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	n := c.calls.Add(1)
	if n <= c.busyUntilCall {
		return nil, session.ErrCallQueuedNotExecuted
	}
	var result any = "ok"
	return &result, nil
}

// TestReleaseGate_P350_QueuedNotExecutedBacksOffWithoutAttemptPenalty proves
// that an entry blocked purely by ErrCallQueuedNotExecuted (a genuinely
// external live owner) survives far more than RunQueueMaxAttempts local
// backoff cycles without being dead-lettered (deleted), and is executed
// successfully once the external owner frees up.
//
// Runs on a fake clock (RunQueuePumpConfig.TestClock): each cycle the test
// advances one backoff window (a TTL of fake time), so the pump re-attempts
// exactly once per step. That also makes attempts == 0 exact after every
// cycle: no real-time cleanup pass can interleave between a lease and its
// Nack (the flake the previous real-clock version documented), because
// cleanup only ever runs at the instants the test advances to.
//
// REVERT CHECK PROCEDURE:
//  1. In run_queue_entry_exec.go's executeEntry, replace the
//     NackRunQueueEntryNoAttemptPenalty + busyBackoffUntil branch under
//     `errors.Is(err, ErrCallQueuedNotExecuted)` with a no-op (or restore
//     the single RenewRunQueueLease call — either reproduces the bug).
//  2. Run: go test -run TestReleaseGate_P350_QueuedNotExecutedBacksOffWithoutAttemptPenalty -v
//  3. FAIL: the row is not released back to pending with attempts 0.
//  4. Restore the fix and PASS.
func TestReleaseGate_P350_QueuedNotExecutedBacksOffWithoutAttemptPenalty(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "test-session-queued-backoff")
	ctx := t.Context()

	const entryID = "queued-backoff-probe"
	callData := map[string]any{"SessionID": sess.ID, "Prompt": "test prompt"}
	callDataJSON, err := json.Marshal(callData)
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(ctx, entryID, sess.ID, callDataJSON))

	// Far more than RunQueueMaxAttempts (10) — if ErrCallQueuedNotExecuted
	// still counted as an attempt (the pre-fix behavior), the entry would
	// be dead-lettered long before reaching this many cycles.
	coord := &queuedNotExecutedThenSuccessCoordinator{busyUntilCall: 25}

	const ttl = time.Hour // fake time; also the local backoff length
	clk := newFakePumpClock(fakePumpEpoch)
	probe := newFakeClockService(svc, clk)
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       probe,
		Coordinator:    coord,
		PumpInstanceID: "queued-backoff-pump",
		TestTick:       func() time.Duration { return ttl / 10 },
		TestLeaseTTL:   ttl,
		TestClock:      clk,
	})
	pump.Start()
	t.Cleanup(func() { pump.Stop() })

	for cycle := int64(1); cycle <= coord.busyUntilCall; cycle++ {
		awaitPump(t, func() bool { return coord.calls.Load() >= cycle }, "the pump must re-attempt the entry each backoff window")
		awaitPump(t, func() bool {
			e, getErr := svc.GetRunQueueEntry(ctx, entryID)
			return getErr == nil && e != nil && e.Status == "pending"
		}, "a busy outcome must release the row back to pending, not dead-letter it")
		entry, getErr := svc.GetRunQueueEntry(ctx, entryID)
		require.NoError(t, getErr)
		require.NotNil(t, entry)
		require.Equal(t, int64(0), entry.Attempts, "cycle %d: ErrCallQueuedNotExecuted must never cost an attempt", cycle)

		// Elapse the local backoff. The tick that consumes this may run
		// before the previous execution has released its in-flight slot
		// (a few instructions after the Nack), and is then skipped; keep
		// ticking by a millisecond until the retry lands.
		clk.Advance(ttl)
		awaitPump(t, func() bool {
			if coord.calls.Load() > cycle {
				return true
			}
			clk.Advance(time.Millisecond)
			return false
		}, "the entry must be attempted again once its backoff elapsed")
	}

	// The 26th call succeeds; the row must be acked, not left behind.
	awaitPump(t, func() bool {
		gone, checkErr := runQueueGoneEverywhere(ctx, svc)
		return checkErr == nil && gone
	}, "entry should be acked once the external owner frees up")
	require.Greater(t, coord.calls.Load(), coord.busyUntilCall)
}
