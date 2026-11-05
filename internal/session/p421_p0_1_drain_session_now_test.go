package session_test

// Focused unit coverage for RunQueuePump.DrainSessionNow (task #421/P0-1),
// complementing the slower, full end-to-end regression test in
// internal/app (TestRunNonInteractive_P0_1_LiveContinuation, which exercises
// the real interrupt ticker + RunNonInteractive wiring but necessarily waits
// out the real 3s interruptInjectTick). These tests isolate DrainSessionNow's
// own three observable outcomes directly against a real RunQueuePump/DB,
// without needing a real interrupt or a real Coordinator.

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestDrainSessionNow_NothingPending verifies the no-op contract:
// DrainSessionNow must not treat "nothing pending" as having recovered
// anything (result=DrainNoWork, err=nil). RunNonInteractive relies on this
// to leave a genuine user/--timeout cancellation (no durable continuation)
// alone instead of fabricating a success.
func TestDrainSessionNow_NothingPending(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "drain-now-nothing-pending")
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    &mockCoordinator{},
		PumpInstanceID: "test-pump-drain-empty",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := pump.DrainSessionNow(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, session.DrainNoWork, result, "nothing was pending — must not report anything drained")
}

// TestDrainSessionNow_ExecutesPendingEntry verifies the core contract: a
// durably enqueued entry for the session is leased, executed, and acked
// synchronously by DrainSessionNow itself — without waiting for a
// background tick — exactly the property RunNonInteractive depends on to
// finish a cross-process interrupt's continuation before the process exits.
func TestDrainSessionNow_ExecutesPendingEntry(t *testing.T) {
	t.Parallel()
	limitParallel(t)
	sess, svc := setupTestSession(t, "drain-now-executes")
	coord := &countingCoordinatorForDrain{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-drain-exec",
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "drain-now-idem-1", sess.ID, callData))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := pump.DrainSessionNow(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, session.DrainComplete, result)
	require.Equal(t, int64(1), coord.calls.Load(), "the pending entry must have been executed exactly once")

	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Empty(t, pending, "the executed entry must be acked/removed, not left pending")
}

// TestDrainSessionNow_WaitsForConcurrentBackgroundTick verifies the
// race-closing path documented on DrainSessionNow: if this SAME pump
// instance's own background tick wins the lease race for the pending entry
// (fires concurrently with — and just ahead of — DrainSessionNow's own
// attempt), DrainSessionNow must wait for that execution to finish and
// still report DrainComplete, rather than concluding "nothing to drain"
// just because ITS OWN lease attempt found the row already taken by a
// goroutine it didn't start.
func TestDrainSessionNow_WaitsForConcurrentBackgroundTick(t *testing.T) {
	tl := newTestTimeline()
	var sess *session.Session
	var svc session.Service
	tl.stepf(func() { sess, svc = setupTestSession(t, "drain-now-race") }, "fixture setup")
	coord := &slowCoordinatorForDrain{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	registerPumpCalls(t, "slow", &coord.calls)
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-drain-race",
		TestTick:       func() time.Duration { return 5 * time.Millisecond },
	})
	pump.Start()
	tl.addf("pump started")
	stopPumpLoggingForcedShutdown(t, pump)
	reportPumpOnFailure(t, tl)

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	tl.stepf(func() {
		require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "drain-now-race-idem-1", sess.ID, callData))
	}, "enqueue")

	// Wait for the BACKGROUND tick (not this test's later DrainSessionNow
	// call) to win the lease and start executing — confirms the race this
	// test targets is actually set up before DrainSessionNow ever runs. A
	// hang guard, not a timing window: how long the tick takes depends on
	// how fast the disk is, which is not what this test measures.
	select {
	case <-coord.started:
		tl.addf("background tick started executing")
	case <-time.After(30 * time.Second):
		t.Fatal("background tick never started executing — test setup is broken, proves nothing")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drainDone := make(chan struct{})
	var result session.DrainResult
	var drainErr error
	go func() {
		defer close(drainDone)
		tl.addf("DrainSessionNow called")
		result, drainErr = pump.DrainSessionNow(ctx, sess.ID)
		tl.addf("DrainSessionNow returned result=%v err=%v", result, drainErr)
	}()

	// Give DrainSessionNow time to find nothing pending (already leased by
	// the background tick) and enter its inFlight poll loop before
	// releasing the background execution.
	time.Sleep(50 * time.Millisecond)
	tl.addf("releasing the background execution")
	close(coord.release)

	select {
	case <-drainDone:
	case <-time.After(30 * time.Second):
		t.Fatal("DrainSessionNow did not return after the concurrent background execution finished")
	}

	require.NoError(t, drainErr)
	require.Equal(t, session.DrainComplete, result, "DrainSessionNow must report DrainComplete when the background tick executed the entry, even though its own lease attempt found nothing pending")
	require.Equal(t, int64(1), coord.calls.Load(), "the entry must have executed exactly once (by the background tick), not a second time via DrainSessionNow")
}

type countingCoordinatorForDrain struct {
	calls atomic.Int64
}

func (c *countingCoordinatorForDrain) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	return nil, nil
}

type slowCoordinatorForDrain struct {
	calls   atomic.Int64
	started chan struct{}
	release chan struct{}
}

func (c *slowCoordinatorForDrain) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	select {
	case c.started <- struct{}{}:
	default:
	}
	select {
	case <-c.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, nil
}
