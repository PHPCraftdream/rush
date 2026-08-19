package session_test

// Regression coverage for P0-2 of the 2026-08-18 release-readiness review
// (task #551): the synchronous drain path used to ignore its context and to
// run outside the pump's shutdown accounting.
//
// Both defects lived in the same seam — DrainSessionNow drives
// executeEntrySync directly rather than going through processEntry — so
// neither was caught by the existing background-dispatch tests, which
// exercise the SAME executeEntrySync through a caller that did get both
// things right.

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestDrainSessionNow_CallerCancellationStopsExecution proves the context
// half of P0-2: cancelling the context passed to DrainSessionNow must cancel
// the in-flight Coordinator.Run.
//
// Before the fix, executeEntrySync built its execution context with
// context.WithCancel(context.Background()) and never read its ctx parameter
// at all — the function compiled with the parameter entirely unused. A
// `crush run --timeout` that expired mid-continuation therefore returned an
// error to the operator while the turn kept running and kept writing
// messages behind it.
//
// Revert-check: restore context.Background() as the parent and this test
// fails on the 5s guard below — the coordinator never observes the cancel.
func TestDrainSessionNow_CallerCancellationStopsExecution(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "drain-now-ctx-cancel")
	coord := &ctxObservingCoordinatorForDrain{
		started: make(chan struct{}, 1),
		sawDone: make(chan struct{}),
	}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-drain-ctx",
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "drain-ctx-idem-1", sess.ID, callData))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		_, _ = pump.DrainSessionNow(ctx, sess.ID)
	}()

	select {
	case <-coord.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain never reached Coordinator.Run — test setup is broken, proves nothing")
	}

	cancel()

	select {
	case <-coord.sawDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Coordinator.Run never observed the caller's cancellation: the execution context is not derived from DrainSessionNow's ctx")
	}

	select {
	case <-drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("DrainSessionNow did not return after its context was cancelled")
	}
}

// TestDrainSessionNow_StopWaitsForSynchronousDrain proves the lifecycle half
// of P0-2: Stop() must not report shutdown complete while a synchronous
// drain is still inside Coordinator.Run.
//
// Before the fix, DrainSessionNow drove executeEntrySync without ever
// registering in workerWg — the only thing Stop() waits on for workers. A
// shutdown racing a drain would therefore see an idle pump and let
// App.Shutdown close the database underneath a live execution.
//
// Ordering note: the pump is started only AFTER the drain is confirmed to be
// executing. That is deliberate and is what makes the test deterministic
// rather than a coin flip — run()'s initial tick fires immediately on Start()
// and would otherwise race the drain for the same row, and a background-
// dispatched execution DOES register in workerWg, so it would satisfy the
// assertion below for the wrong reason. Starting late leaves the initial tick
// nothing to find (the row is already leased and the session is already
// marked in flight), which pins the drain as the sole executor — asserted by
// the call count.
//
// Revert-check: remove the workerWg.Add/Done pair from DrainSessionNow and
// this test fails immediately at the "returned early" guard.
func TestDrainSessionNow_StopWaitsForSynchronousDrain(t *testing.T) {
	sess, svc := setupTestSession(t, "drain-now-stop-waits")
	coord := &slowCoordinatorForDrain{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-drain-stop",
		// Long enough that only run()'s initial tick ever fires, and that
		// tick happens after the drain already owns the row.
		TestTick: func() time.Duration { return time.Hour },
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "drain-stop-idem-1", sess.ID, callData))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		_, _ = pump.DrainSessionNow(ctx, sess.ID)
	}()

	select {
	case <-coord.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain never reached Coordinator.Run — test setup is broken, proves nothing")
	}

	pump.Start()

	stopReturned := make(chan bool, 1)
	go func() { stopReturned <- pump.Stop() }()

	// The drain is parked inside Coordinator.Run and stays there until
	// released below. Stop() must still be blocked.
	select {
	case <-stopReturned:
		t.Fatal("Stop() returned while a synchronous drain was still inside Coordinator.Run: the drain is not registered in the pump's shutdown accounting")
	case <-time.After(200 * time.Millisecond):
	}

	close(coord.release)

	select {
	case forced := <-stopReturned:
		require.False(t, forced, "Stop() should have completed gracefully once the drain finished, not timed out into a forced shutdown")
	case <-time.After(10 * time.Second):
		t.Fatal("Stop() never returned after the drain finished")
	}

	select {
	case <-drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("DrainSessionNow never returned")
	}

	require.Equal(t, int64(1), coord.calls.Load(), "the entry must have been executed exactly once, by the drain — a second call means the background tick raced it and this test proved nothing about the drain path")
}

// TestDrainSessionNow_DeclinesToStartOnceStopping proves the other side of
// the same admission gate: a drain that arrives after Stop() has flipped the
// gate must not begin an execution Stop() has already decided not to wait
// for. It releases the lease instead, so a later process recovers the entry
// normally rather than waiting out a full lease TTL.
func TestDrainSessionNow_DeclinesToStartOnceStopping(t *testing.T) {
	sess, svc := setupTestSession(t, "drain-now-stopping")
	coord := &countingCoordinatorForDrain{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-drain-stopping",
		TestTick:       func() time.Duration { return time.Hour },
	})
	pump.Start()
	require.False(t, pump.Stop(), "the pump had no work — shutdown must be graceful")

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "drain-stopping-idem-1", sess.ID, callData))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = pump.DrainSessionNow(ctx, sess.ID)
	require.ErrorIs(t, err, session.ErrCallQueuedNotExecuted, "a drain arriving after shutdown must report that it did not execute the call")
	require.Equal(t, int64(0), coord.calls.Load(), "no execution may start once the admission gate is closed")

	// The lease must have been released without an attempt penalty, so the
	// entry is immediately recoverable rather than stuck 'leased' for a TTL.
	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Len(t, pending, 1, "the declined entry must be back to pending, not left leased")
	require.Equal(t, int64(0), pending[0].Attempts, "declining to start is not a failed attempt")
}

type ctxObservingCoordinatorForDrain struct {
	calls   atomic.Int64
	started chan struct{}
	sawDone chan struct{}
}

func (c *ctxObservingCoordinatorForDrain) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	select {
	case c.started <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		close(c.sawDone)
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		// Bounded so a failing run reports the assertion above rather than
		// hanging the package until the go test timeout.
		return nil, nil
	}
}
