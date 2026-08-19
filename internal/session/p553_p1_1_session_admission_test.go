package session_test

// Regression coverage for P1-1 of the 2026-08-18 release-readiness review
// (task #553): the per-session in-flight marker could not represent two
// dispatch paths.
//
// processEntry checked the marker, leased, then set it — safe only against
// tick()'s own single-threaded loop. DrainSessionNow leased FIRST and then
// assigned the same key unconditionally, from another goroutine. So a
// background worker running row A for session S and a drain running row B
// for the same S both believed one boolean represented them, and whichever
// finished first deleted it, leaving the other running while the session
// looked free.

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// concurrencyProbeCoordinator records the highest number of Run calls that
// were ever in flight simultaneously. That number IS the invariant under
// test: one execution per session per pump instance.
type concurrencyProbeCoordinator struct {
	calls    atomic.Int64
	live     atomic.Int64
	maxLive  atomic.Int64
	started  chan struct{}
	release  chan struct{}
	holdOnce atomic.Bool
}

func (c *concurrencyProbeCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	live := c.live.Add(1)
	for {
		old := c.maxLive.Load()
		if live <= old || c.maxLive.CompareAndSwap(old, live) {
			break
		}
	}
	defer c.live.Add(-1)

	// Only the FIRST call parks. The second (if the bug lets one start
	// concurrently) returns immediately, so a broken build fails fast on the
	// recorded maximum instead of deadlocking the test.
	if c.holdOnce.CompareAndSwap(false, true) {
		select {
		case c.started <- struct{}{}:
		default:
		}
		select {
		case <-c.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, nil
}

// TestDrainSessionNow_DoesNotRunConcurrentlyWithBackgroundTick proves that a
// drain cannot start a second execution for a session this pump instance is
// already executing, and that the two paths share one admission gate rather
// than one boolean each believes it owns.
//
// Revert-check: move DrainSessionNow's session marking back below
// LeaseRunQueueEntry (lease first, then assign p.inFlight unconditionally)
// and this fails on maxLive — the drain leases the second entry and runs it
// while the background worker is still inside Coordinator.Run.
func TestDrainSessionNow_DoesNotRunConcurrentlyWithBackgroundTick(t *testing.T) {
	sess, svc := setupTestSession(t, "admission-one-per-session")
	coord := &concurrencyProbeCoordinator{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-admission",
		TestTick:       func() time.Duration { return 5 * time.Millisecond },
	})
	pump.Start()
	stopPumpLoggingForcedShutdown(t, pump)

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	// TWO entries for the SAME session — the shape the marker could not
	// represent. One is not enough: with a single row the drain and the tick
	// contend for it at the DB level and only one can ever win.
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "admission-idem-1", sess.ID, callData))
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "admission-idem-2", sess.ID, callData))

	// Wait for the BACKGROUND tick to be inside Coordinator.Run before the
	// drain starts — otherwise this test proves nothing about the overlap.
	select {
	case <-coord.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the background tick never started executing — test setup is broken")
	}

	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = pump.DrainSessionNow(ctx, sess.ID)
	}()

	// Give the drain time to do the wrong thing if it can: lease the second
	// entry and start executing it alongside the parked background worker.
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, int64(1), coord.maxLive.Load(),
		"two executions ran for one session at the same time — the drain bypassed the admission the background tick holds")

	close(coord.release)

	select {
	case <-drainDone:
	case <-time.After(10 * time.Second):
		t.Fatal("DrainSessionNow never returned after the background execution finished")
	}

	require.Equal(t, int64(1), coord.maxLive.Load(), "never more than one execution per session")
	require.GreaterOrEqual(t, coord.calls.Load(), int64(2), "both entries must eventually execute, just not at once")
}

// TestAdmitSession_IsExclusiveAndOnlyItsOwnerReleases pins the two
// properties the shared gate provides, directly rather than through a race.
//
// The second is the one the old code violated: a marker set by one execution
// could be deleted by another, because clearing it was an open-coded
// `delete(p.inFlight, sessionID)` available to anyone. Here the only way to
// clear it is the closure handed to whoever was admitted.
func TestAdmitSession_IsExclusiveAndOnlyItsOwnerReleases(t *testing.T) {
	t.Parallel()
	_, svc := setupTestSession(t, "admission-unit")
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    &countingCoordinatorForDrain{},
		PumpInstanceID: "test-pump-admission-unit",
	})

	release, ok := pump.AdmitSessionForTest("s1")
	require.True(t, ok, "a free session must be admitted")

	_, ok2 := pump.AdmitSessionForTest("s1")
	require.False(t, ok2, "a second admission for the same session must be refused, not granted a duplicate marker")

	releaseOther, okOther := pump.AdmitSessionForTest("s2")
	require.True(t, okOther, "a different session must be unaffected")
	releaseOther(nil)

	release(nil)
	release(nil) // idempotent: call sites hand this closure between functions

	regained, ok3 := pump.AdmitSessionForTest("s1")
	require.True(t, ok3, "once released, the session must be admissible again")
	regained(nil)
}
