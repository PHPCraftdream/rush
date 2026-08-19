package session_test

// Regression coverage for task #575 of the 2026-08-19 release-readiness
// review: DrainSessionNow, on losing the admission race to a same-pump
// background worker, used to set drained=true the moment admission cleared
// — without ever learning what that worker's execution actually produced.
// "Admission cleared" is true for FIVE different real outcomes, and only
// ONE of them (a clean commit) may legitimately produce a completed drain:
//
//  1. ErrCallQueuedNotExecuted / SessionLockBusyError — busy, nothing ran.
//  2. A terminal AlreadyAttempted failure.
//  3. A failed Ack (turn ran, commit did not).
//  4. A lost lease (a new owner now holds the row).
//  5. A genuine committed success.
//
// Each test below forces DrainSessionNow to lose the admission race for a
// SECOND pending entry against a background tick already executing a FIRST
// entry for the same session, and pins what DrainSessionNow reports once
// that background execution resolves.

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// gatedCoordinator lets a test control exactly when the background
// execution's Coordinator.Run call returns, and what it returns, so the
// admission race between the background tick and a concurrent
// DrainSessionNow call can be forced deterministically instead of hoping
// timing lines up.
type gatedCoordinator struct {
	calls   atomic.Int64
	started chan struct{}
	release chan error // send the desired Run() return value to unblock
}

func newGatedCoordinator() *gatedCoordinator {
	return &gatedCoordinator{
		started: make(chan struct{}, 1),
		release: make(chan error),
	}
}

func (c *gatedCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	select {
	case c.started <- struct{}{}:
	default:
	}
	select {
	case err := <-c.release:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// oneEntrySetup enqueues a SINGLE durable entry for a fresh session and
// starts a pump with a fast background tick, returning everything a test
// needs to force the race: the background tick claims the only entry, so
// a concurrent DrainSessionNow call is guaranteed to find nothing left to
// lease itself and must go through the wait-for-admission branch under
// test. One entry (not two, unlike the exclusivity test in
// p553_p1_1_session_admission_test.go) is deliberate here: these tests care
// about what DrainSessionNow reports for the outcome it WAITED for, not
// about whether it goes on to execute further work itself, and a second
// pending entry would force gatedCoordinator.Run to be entered again after
// the wait — needing a second value on gate.release that these tests have
// no reason to send.
func oneEntrySetup(t *testing.T, name string, gate *gatedCoordinator) (*session.RunQueuePump, string) {
	t.Helper()
	sess, svc := setupTestSession(t, name)
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    gate,
		PumpInstanceID: "test-pump-" + name,
		TestTick:       func() time.Duration { return 5 * time.Millisecond },
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), name+"-idem-1", sess.ID, callData))

	pump.Start()
	stopPumpLoggingForcedShutdown(t, pump)

	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background tick never started executing — test setup is broken, proves nothing about the race under test")
	}

	return pump, sess.ID
}

// TestDrainSessionNow_WaitedExecution_BusyIsNotDrained pins outcome (1): the
// background worker's Run() reports the session was busy elsewhere
// (ErrCallQueuedNotExecuted). A DrainSessionNow call that loses the
// admission race and waits for that outcome must NOT report a completed
// drain — nothing actually ran.
//
// Revert-check: replace classifyBackgroundOutcome's use in the wait branch
// with the pre-fix behavior (treat admission clearing as an unconditional
// success) and this fails on the require.Equal below with DrainComplete
// reported instead of DrainNoWork — the exact false success task #575
// exists to close.
func TestDrainSessionNow_WaitedExecution_BusyIsNotDrained(t *testing.T) {
	gate := newGatedCoordinator()
	pump, sessID := oneEntrySetup(t, "wait-busy", gate)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drainDone := make(chan struct{})
	var result session.DrainResult
	var drainErr error
	go func() {
		defer close(drainDone)
		result, drainErr = pump.DrainSessionNow(ctx, sessID)
	}()

	// Give DrainSessionNow time to lose the admission race and enter the
	// wait branch before resolving the background execution.
	time.Sleep(150 * time.Millisecond)
	gate.release <- session.ErrCallQueuedNotExecuted

	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSessionNow did not return after the observed background execution resolved")
	}

	require.NoError(t, drainErr, "an externally-owned session is ordinary contention, not a failure of this call")
	require.Equal(t, session.DrainNoWork, result, "nothing ran for the observed execution — reporting a completed/partial/failed drain here is the exact false success task #575 exists to close")
}

// TestDrainSessionNow_WaitedExecution_TerminalFailureIsSurfaced pins outcome
// (2): the background worker's turn terminal-fails (AlreadyAttempted). The
// waiting DrainSessionNow call must report DrainFailed (an execution DID
// happen and reach a resolution) AND surface the terminal error — not
// silently report success.
//
// Revert-check: same as above — without routing the wait branch's observed
// outcome through classifyBackgroundOutcome, DrainSessionNow returns
// DrainComplete/nil here instead of DrainFailed/non-nil, which is a false
// success over a terminally-failed row.
func TestDrainSessionNow_WaitedExecution_TerminalFailureIsSurfaced(t *testing.T) {
	gate := newGatedCoordinator()
	pump, sessID := oneEntrySetup(t, "wait-terminal", gate)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drainDone := make(chan struct{})
	var result session.DrainResult
	var drainErr error
	go func() {
		defer close(drainDone)
		result, drainErr = pump.DrainSessionNow(ctx, sessID)
	}()

	time.Sleep(150 * time.Millisecond)
	termErr := &agent.ErrCallAlreadyAttempted{Err: errors.New("provider already charged for this call")}
	gate.release <- termErr

	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSessionNow did not return after the observed background execution resolved")
	}

	require.Error(t, drainErr, "the observed execution terminal-failed; reporting success would hide a row that will never be retried")
	require.ErrorIs(t, drainErr, termErr)
	require.Equal(t, session.DrainFailed, result, "an execution DID happen and reach a resolution in this process, even though it was a terminal failure")
}

// TestDrainSessionNow_WaitedExecution_AckFailureIsSurfaced pins outcome (3):
// the background worker's turn runs and commits, but AckRunQueueEntry fails
// — the row stays leased, not deleted. The waiting DrainSessionNow call must
// surface ErrTurnCommitFailed, not silently report success for a turn that
// was never actually committed.
func TestDrainSessionNow_WaitedExecution_AckFailureIsSurfaced(t *testing.T) {
	sess, svc := setupTestSession(t, "wait-ack-failure")
	failing := &ackFailingServiceP575{Service: svc, ackErr: errors.New("disk on fire")}
	gate := newGatedCoordinator()
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       failing,
		Coordinator:    gate,
		PumpInstanceID: "test-pump-wait-ack-failure",
		TestTick:       func() time.Duration { return 5 * time.Millisecond },
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	// A SINGLE entry, deliberately — see oneEntrySetup's doc for why a
	// second pending entry would need a second value on gate.release that
	// this test has no reason to send.
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "wait-ack-failure-idem-1", sess.ID, callData))

	pump.Start()
	stopPumpLoggingForcedShutdown(t, pump)

	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background tick never started executing — test setup is broken, proves nothing about the race under test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drainDone := make(chan struct{})
	var result session.DrainResult
	var drainErr error
	go func() {
		defer close(drainDone)
		result, drainErr = pump.DrainSessionNow(ctx, sess.ID)
	}()

	time.Sleep(150 * time.Millisecond)
	gate.release <- nil // the turn itself succeeds; the Ack write is what fails

	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSessionNow did not return after the observed background execution resolved")
	}

	require.Error(t, drainErr, "the observed turn ran but its Ack failed — the row was never actually committed")
	require.ErrorIs(t, drainErr, session.ErrTurnCommitFailed)
	require.Equal(t, session.DrainFailed, result, "the turn DID execute in this process; it is only the commit that failed")
}

// ackFailingServiceP575 wraps a real session.Service and fails
// AckRunQueueEntry exactly like p552's ackFailingService — duplicated here
// (rather than reused) to keep this file's regression self-contained and
// because Go test files in the same package cannot forward-reference a
// helper defined for an unrelated task's file without coupling the two.
type ackFailingServiceP575 struct {
	session.Service
	ackErr error
}

func (s *ackFailingServiceP575) AckRunQueueEntry(ctx context.Context, id, leasedBy string) (string, error) {
	return "", s.ackErr
}

// leaseStealingService wraps a real session.Service and makes every
// RenewRunQueueLease call report that the lease was reassigned to a
// different owner (ok=false, err=nil) — forcing executeEntrySync's renewal
// loop down its errLeaseLost path deterministically, without needing to
// wait out a real TTL window or coordinate a second live pump instance.
type leaseStealingService struct {
	session.Service
}

func (s *leaseStealingService) RenewRunQueueLease(ctx context.Context, id, leasedBy string, newExpiresAt int64) (bool, error) {
	return false, nil
}

// TestDrainSessionNow_WaitedExecution_LeaseLossIsSurfaced pins outcome (4):
// the background worker loses its lease to a different owner mid-execution.
// The waiting DrainSessionNow call must surface that as a non-nil error
// (errLeaseLost), never as a bare success — the row's fate now belongs to
// whoever re-leased it, and this process learned nothing it can act on.
func TestDrainSessionNow_WaitedExecution_LeaseLossIsSurfaced(t *testing.T) {
	sess, svc := setupTestSession(t, "wait-lease-loss")
	stealing := &leaseStealingService{Service: svc}
	gate := newGatedCoordinator()
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       stealing,
		Coordinator:    gate,
		PumpInstanceID: "test-pump-wait-lease-loss",
		TestTick:       func() time.Duration { return 5 * time.Millisecond },
		// A short TTL makes the renewal loop attempt its first (rejected)
		// renewal quickly instead of waiting out the 30s production TTL/3.
		TestLeaseTTL: 60 * time.Millisecond,
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	// A SINGLE entry — see oneEntrySetup's doc.
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "wait-lease-loss-idem-1", sess.ID, callData))

	pump.Start()
	stopPumpLoggingForcedShutdown(t, pump)

	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background tick never started executing — test setup is broken, proves nothing about the race under test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drainDone := make(chan struct{})
	var result session.DrainResult
	var drainErr error
	go func() {
		defer close(drainDone)
		result, drainErr = pump.DrainSessionNow(ctx, sess.ID)
	}()

	// Give DrainSessionNow time to lose the admission race and enter the
	// wait branch. The background execution's own watchdog/renewal loop
	// will independently drive it to lease loss and execCancel() — the
	// gate's Run() call observes ctx.Done() and returns on its own, so
	// nothing needs to be sent on gate.release here.
	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSessionNow did not return after the observed background execution lost its lease")
	}

	require.Error(t, drainErr, "the observed execution lost its lease mid-run — a new owner holds the row now, and this process learned nothing it can act on")
	require.ErrorContains(t, drainErr, "lost lease ownership", "the observed execution's lease-loss outcome must be surfaced, not silently treated as success")
	require.Equal(t, session.DrainFailed, result, "an execution DID happen and reach SOME resolution (lease loss) in this process")
}

// TestDrainSessionNow_WaitedExecution_SuccessIsDrained is the control for
// all four tests above: the ONE outcome that may legitimately produce a
// completed drain is a genuine committed success. Losing the admission race
// must not, by itself, prevent DrainSessionNow from correctly reporting a
// success it merely observed rather than executed itself.
func TestDrainSessionNow_WaitedExecution_SuccessIsDrained(t *testing.T) {
	gate := newGatedCoordinator()
	pump, sessID := oneEntrySetup(t, "wait-success", gate)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drainDone := make(chan struct{})
	var result session.DrainResult
	var drainErr error
	go func() {
		defer close(drainDone)
		result, drainErr = pump.DrainSessionNow(ctx, sessID)
	}()

	time.Sleep(150 * time.Millisecond)
	gate.release <- nil

	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSessionNow did not return after the observed background execution resolved")
	}

	require.NoError(t, drainErr)
	require.Equal(t, session.DrainComplete, result, "the observed execution committed cleanly — this is the one outcome that must report a completed drain")
}
