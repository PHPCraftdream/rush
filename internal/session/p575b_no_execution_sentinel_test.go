package session_test

// Regression coverage for the hole in task #575's first pass, found by the
// coordinator's line-by-line review: admitSession's release closure takes an
// outcome error, but every early-return path in processEntry (and
// DrainSessionNow's own pre-execution paths) was publishing a bare nil for
// "nothing executed". nil is ALSO executeEntrySync's own return value for a
// clean commit, so classifyBackgroundOutcome could not tell "an execution
// committed" apart from "no execution was ever attempted" — a concurrent
// DrainSessionNow waiting on that admission would read either as success.
//
// The fix introduces errNoExecutionAttempted and routes every early-return
// release through it instead of nil. These two tests drive that through
// REAL production dispatch (a real background processEntry call via
// pump.Start(), and a real concurrent DrainSessionNow), not by calling the
// release closure by hand — the point the coordinator raised is that the
// PRODUCTION call sites publish the right thing, not just that the sentinel
// classifies correctly in isolation (already covered by
// classifyBackgroundOutcome's own unit shape via the WaitedExecution tests).

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// leaseGateAction controls what a parked delayedRaceLeaseService.
// LeaseRunQueueEntry call does once released.
type leaseGateAction int

const (
	// leaseGateRace simulates a DIFFERENT pump instance having already
	// leased the row first: returns (nil, nil) without touching the real
	// service — processEntry's own "leased == nil" early return.
	leaseGateRace leaseGateAction = iota
	// leaseGateProceedWithCallerCtx forwards to the real
	// LeaseRunQueueEntry using the ctx processEntry itself passed in —
	// which may already be cancelled if Stop() ran first, exercising
	// processEntry's "lease failed" early return instead of the
	// p.stopping branch.
	leaseGateProceedWithCallerCtx
	// leaseGateProceedWithFreshCtx forwards to the real
	// LeaseRunQueueEntry using a fresh context.Background(), independent
	// of whatever the caller's ctx is doing. Used specifically to reach
	// processEntry's p.stopping branch: that branch requires the lease to
	// SUCCEED (leased != nil) so the p.stopping check downstream is even
	// reached, but Stop() cancels p.ctx (and therefore tick()'s ctx,
	// which processEntry's own ctx parameter is derived from)
	// synchronously, right after flipping p.stopping — a real caller-ctx
	// lease attempted after Stop() has been triggered would usually fail
	// with "context canceled" instead of succeeding, which would exercise
	// a DIFFERENT early return (processEntry's plain "lease failed"
	// branch, itself already covered by leaseGateProceedWithCallerCtx)
	// rather than the p.stopping branch this test specifically targets.
	leaseGateProceedWithFreshCtx
)

// delayedRaceLeaseService wraps a real session.Service so its
// LeaseRunQueueEntry call can be held open on a gate before resolving in one
// of three ways (see leaseGateAction), letting a test control exactly which
// of processEntry's early-return paths a background dispatch takes.
//
// It also records every NackRunQueueEntryNoAttemptPenalty reason string, so
// a test can positively confirm WHICH early return actually fired — the
// p.stopping branch is the only processEntry path that nacks with the exact
// reason "run_queue_pump: shutting down, releasing lease without
// executing"; the raced leased==nil branch never calls Nack at all (it
// returns before ever leasing anything to release). Without this,
// "the row is back in pending with 0 attempts" is true of BOTH branches and
// cannot tell a test which one it actually exercised.
type delayedRaceLeaseService struct {
	session.Service
	// gate, if non-nil, blocks the FIRST LeaseRunQueueEntry call until an
	// action is sent on it.
	gate    chan leaseGateAction
	entered chan struct{} // signaled once, right as the first call parks on gate
	calls   atomic.Int64

	nackMu      sync.Mutex
	nackReasons []string
}

func (s *delayedRaceLeaseService) NackRunQueueEntryNoAttemptPenalty(ctx context.Context, id, leasedBy, reason string) error {
	s.nackMu.Lock()
	s.nackReasons = append(s.nackReasons, reason)
	s.nackMu.Unlock()
	return s.Service.NackRunQueueEntryNoAttemptPenalty(ctx, id, leasedBy, reason)
}

func (s *delayedRaceLeaseService) sawStoppingNack() bool {
	s.nackMu.Lock()
	defer s.nackMu.Unlock()
	for _, r := range s.nackReasons {
		if r == "run_queue_pump: shutting down, releasing lease without executing" {
			return true
		}
	}
	return false
}

func (s *delayedRaceLeaseService) LeaseRunQueueEntry(ctx context.Context, sessionID, leasedBy string, leaseTTL time.Duration) (*session.RunQueueEntry, error) {
	n := s.calls.Add(1)
	if n == 1 && s.gate != nil {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		switch <-s.gate {
		case leaseGateRace:
			// A DIFFERENT pump instance races us and ACTUALLY leases the
			// row first — a real, independent LeaseRunQueueEntry call
			// under a different leasedBy, on the real underlying service,
			// not just a faked (nil, nil) return with the row still
			// sitting pending underneath. This matters for what a waiting
			// DrainSessionNow does next: with the row genuinely gone from
			// pending, DrainSessionNow's post-#575-round-2 fix (waiting on
			// this outcome must retry admission rather than stop
			// immediately — see classifyBackgroundOutcome's
			// errNoExecutionAttempted branch) correctly finds NOTHING
			// left to lease on its retry and reports drained=false
			// truthfully. A faked (nil, nil) with the row still pending
			// would instead let the waiter legitimately re-lease and
			// execute the very same row itself — a DIFFERENT, correct
			// outcome that this test must not conflate with the bug it is
			// trying to catch.
			if _, err := s.Service.LeaseRunQueueEntry(context.Background(), sessionID, "a-different-pump-instance", leaseTTL); err != nil {
				return nil, err
			}
			return nil, nil
		case leaseGateProceedWithFreshCtx:
			return s.Service.LeaseRunQueueEntry(context.Background(), sessionID, leasedBy, leaseTTL)
		}
	}
	return s.Service.LeaseRunQueueEntry(ctx, sessionID, leasedBy, leaseTTL)
}

// TestProcessEntry_RacedLeaseNil_DoesNotFalselyDrainAWaiter pins the
// coordinator's concrete false-success scenario: a background processEntry
// call loses the lease race (LeaseRunQueueEntry returns nil, nil — a
// DIFFERENT pump instance took the row first), takes the early return, and
// releases admission. A DrainSessionNow call that lost the ADMISSION race
// against this processEntry call (not the lease race) and is waiting on it
// must NOT read that release as a completed continuation.
//
// Revert-check: change errNoExecutionAttempted back to nil in processEntry's
// "leased == nil" path's admission-defer release value (i.e. reproduce the
// bug the coordinator found) and this fails on the require.False below.
func TestProcessEntry_RacedLeaseNil_DoesNotFalselyDrainAWaiter(t *testing.T) {
	sess, svc := setupTestSession(t, "no-exec-raced-lease")
	wrapped := &delayedRaceLeaseService{
		Service: svc,
		gate:    make(chan leaseGateAction),
		entered: make(chan struct{}, 1),
	}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       wrapped,
		Coordinator:    &countingCoordinatorForDrain{},
		PumpInstanceID: "test-pump-no-exec-raced-lease",
		TestTick:       func() time.Duration { return 5 * time.Millisecond },
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "no-exec-raced-lease-idem-1", sess.ID, callData))

	pump.Start()
	stopPumpLoggingForcedShutdown(t, pump)

	// Wait for the background tick's processEntry to have been admitted and
	// be parked inside LeaseRunQueueEntry, holding admission for this
	// session but not yet having executed (or failed to execute) anything.
	select {
	case <-wrapped.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("background processEntry never reached LeaseRunQueueEntry — test setup is broken, proves nothing")
	}

	// Start a concurrent DrainSessionNow now, while the background
	// processEntry call still holds admission. It must lose the admission
	// race and enter the wait branch.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drainDone := make(chan struct{})
	var drained bool
	var drainErr error
	go func() {
		defer close(drainDone)
		drained, drainErr = pump.DrainSessionNow(ctx, sess.ID)
	}()

	// Give DrainSessionNow time to lose the admission race and start
	// waiting on the background processEntry's admissionEntry before
	// releasing the race simulation.
	time.Sleep(150 * time.Millisecond)
	wrapped.gate <- leaseGateRace // simulate: a different pump instance leased the row first

	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSessionNow did not return after the observed background processEntry call released admission")
	}

	require.NoError(t, drainErr, "nothing executed and nothing failed on this call's own side either")
	require.False(t, drained, "processEntry's raced leased==nil early return executed NOTHING — a waiting DrainSessionNow reporting drained=true here is the coordinator's exact false-success scenario for task #575's first pass")
}

// TestProcessEntry_StoppingMidDispatch_DoesNotFalselyDrainAWaiter pins the
// coordinator's second named scenario: processEntry successfully leases the
// row, but by the time it checks p.stopping (after Stop() has been called
// concurrently), it releases the lease via NackRunQueueEntryNoAttemptPenalty
// and returns WITHOUT ever calling executeEntrySync. A DrainSessionNow call
// waiting on that admission must not be told a continuation completed.
//
// Revert-check: same as above — reproducing the bug (releasing with nil
// instead of errNoExecutionAttempted on this path) makes this fail on the
// require.False below.
func TestProcessEntry_StoppingMidDispatch_DoesNotFalselyDrainAWaiter(t *testing.T) {
	sess, svc := setupTestSession(t, "no-exec-stopping")
	wrapped := &delayedRaceLeaseService{
		Service: svc,
		gate:    make(chan leaseGateAction),
		entered: make(chan struct{}, 1),
	}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       wrapped,
		Coordinator:    &countingCoordinatorForDrain{},
		PumpInstanceID: "test-pump-no-exec-stopping",
		TestTick:       func() time.Duration { return 5 * time.Millisecond },
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "no-exec-stopping-idem-1", sess.ID, callData))

	pump.Start()

	select {
	case <-wrapped.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("background processEntry never reached LeaseRunQueueEntry — test setup is broken, proves nothing")
	}

	// Start the concurrent DrainSessionNow while processEntry is still
	// parked before its lease call resolves, so it loses the admission race
	// and enters the wait branch.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drainDone := make(chan struct{})
	var drained bool
	var drainErr error
	go func() {
		defer close(drainDone)
		drained, drainErr = pump.DrainSessionNow(ctx, sess.ID)
	}()
	time.Sleep(150 * time.Millisecond)

	// Begin Stop() concurrently — it flips p.stopping under admitMu and
	// then cancels + waits. Give it a moment to flip the flag before
	// letting processEntry's lease call proceed. Uses
	// leaseGateProceedWithFreshCtx (context.Background(), not the
	// cancellable ctx processEntry itself received): Stop() cancels p.ctx
	// synchronously right after setting p.stopping = true, so by the time
	// this test's own goroutine schedule lets the lease call resume,
	// p.ctx is essentially certain to already be cancelled — a real
	// caller-ctx lease attempted at that point fails with "context
	// canceled" (processEntry's ordinary "lease failed" early return,
	// already covered by the raced-lease test above) rather than
	// succeeding and reaching the p.stopping check this test targets. A
	// fresh, independent context is what makes landing in the SPECIFIC
	// leased != nil && p.stopping branch deterministic instead of a coin
	// flip between two different early returns.
	stopDone := make(chan bool)
	go func() {
		stopDone <- pump.Stop()
	}()
	time.Sleep(100 * time.Millisecond)
	wrapped.gate <- leaseGateProceedWithFreshCtx

	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSessionNow did not return after the observed background processEntry call released admission")
	}
	<-stopDone

	// Positive confirmation this test actually exercised processEntry's
	// p.stopping branch specifically, and not e.g. the "lease failed"
	// early return the doc above explains this test is trying to avoid.
	// The stopping branch is the ONLY processEntry path that calls Nack
	// with this exact reason string; "row is pending again with 0
	// attempts" alone (checked below) is also true of the raced
	// leased==nil early return and would not disambiguate the two.
	require.True(t, wrapped.sawStoppingNack(), "test setup did not reach processEntry's p.stopping branch — the lease must have failed outright instead (see leaseGateProceedWithFreshCtx's doc); this test proves nothing about the branch it claims to")

	// After observing processEntry's p.stopping release, DrainSessionNow
	// correctly retries admission (errNoExecutionAttempted maps to
	// stopNow=false — see classifyBackgroundOutcome's own doc for why: the
	// row genuinely went back to pending, and a caller that merely lost a
	// race must not conclude "nothing left to do" on someone else's
	// behalf). But p.stopping is ALSO still true for THIS call's own retry
	// by now (Stop() already returned by the time DrainSessionNow's second
	// loop iteration runs), so that retry correctly hits DrainSessionNow's
	// OWN p.stopping branch and returns ErrCallQueuedNotExecuted — a real,
	// meaningful "the pump is shutting down" error, not a fabricated
	// success. drained must still be false: nothing executed across either
	// iteration of this call.
	require.ErrorIs(t, drainErr, session.ErrCallQueuedNotExecuted, "DrainSessionNow's own retry, after correctly falling through instead of stopping early, hits the pump's own p.stopping gate and must report that truthfully")
	require.False(t, drained, "processEntry's p.stopping early return executed NOTHING (the lease was released via Nack, executeEntrySync was never called), and neither did DrainSessionNow's own subsequent retry (also declined due to p.stopping) — a waiting DrainSessionNow reporting drained=true here is the coordinator's exact false-success scenario, reached via the shutdown path instead of a raced lease")

	// The row must have been released back to pending (Nacked, no attempt
	// penalty), not silently lost.
	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Len(t, pending, 1, "the entry must be released back to pending by the stopping-path nack, not consumed")
	require.Equal(t, int64(0), pending[0].Attempts, "release-on-shutdown must not count as an attempt")
}

// TestDrainSessionNow_SecondCallFindsNothingPending_ReportsNotDrained is a
// SANITY check for DrainSessionNow's bottom-of-loop "nothing pending"
// release (reached when leased == nil), not a revert-detecting regression
// test for the nil-vs-errNoExecutionAttempted distinction — said plainly
// because a mislabeled test is worse than no test.
//
// Why it cannot detect the sentinel: this call's outcome is only observable
// by a SECOND, concurrent caller that loses the admission race against THIS
// specific loop iteration and waits on the admissionEntry it publishes. In
// production, that iteration has no gate or delay of any kind — it is
// "lease returned nil, release, return" with no I/O in between — so there
// is no reliable, non-flaky way to land a second goroutine's admission
// attempt inside that exact window without adding a test-only seam to
// production code purely to make this one test observable. Changing the
// release call here from errNoExecutionAttempted back to nil does NOT
// change this test's outcome, because nothing waits on it in this shape.
//
// What this test DOES verify: a first DrainSessionNow call runs the
// session's one pending entry to a genuine success; a second, SEQUENTIAL
// call (not concurrent) correctly finds nothing left pending and reports
// drained=false rather than re-claiming the first call's work. That is real
// coverage for the ordinary, non-racing case, and it would fail if the
// bottom path's classification were wrong in some OTHER way (e.g. if it
// mistakenly set drained=true unconditionally) — it just does not
// distinguish nil from the sentinel specifically, so it is not offered as
// satisfying the coordinator's revert-check requirement for this call site.
// The two processEntry tests above (raced lease and p.stopping) are what
// carry that requirement; this call site's fix is covered by the reasoning
// written at each call site in run_queue_drain_session.go instead of by a
// dedicated concurrency test, per the coordinator's own framing of that
// part of the review as "decide deliberately and write down the reasoning".
func TestDrainSessionNow_SecondCallFindsNothingPending_ReportsNotDrained(t *testing.T) {
	sess, svc := setupTestSession(t, "no-exec-bottom-path")
	coord := &countingCoordinatorForDrain{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-no-exec-bottom-path",
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "no-exec-bottom-path-idem-1", sess.ID, callData))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	firstDrained, firstErr := pump.DrainSessionNow(ctx, sess.ID)
	require.NoError(t, firstErr)
	require.True(t, firstDrained, "the one pending entry must have executed under the first call")
	require.Equal(t, int64(1), coord.calls.Load())

	secondDrained, secondErr := pump.DrainSessionNow(ctx, sess.ID)
	require.NoError(t, secondErr, "nothing pending and nothing failed — this call has nothing to report")
	require.False(t, secondDrained, "the second call found nothing pending (leased == nil, bottom-path release) — it must not report drained=true for work it did not touch")
	require.Equal(t, int64(1), coord.calls.Load(), "the second call must not have re-executed anything")
}
