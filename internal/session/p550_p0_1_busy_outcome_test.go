package session_test

// Regression coverage for P0-1 of the 2026-08-18 release-readiness review
// (task #550): DrainSessionNow reported a success for work it had not run.
//
// The shape of the bug was that `drained` was set the moment
// LeaseRunQueueEntry returned a row — i.e. by the act of holding a lease,
// before any execution was attempted. When execution then turned out to be
// impossible because a different live owner held the session, the row was
// released (correctly, without an attempt penalty) and the function still
// answered (true, nil). RunNonInteractive reads that pair as "the
// continuation ran in this process" and converts the operator's cancelled
// run into exit code 0 with a success envelope.

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// busyCoordinatorForDrain reports that the call was appended to a live
// owner's mailbox rather than executed — the ErrCallQueuedNotExecuted half
// of the "someone else owns this session" outcome.
type busyCoordinatorForDrain struct {
	calls atomic.Int64
}

func (c *busyCoordinatorForDrain) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	return nil, session.ErrCallQueuedNotExecuted
}

// lockBusyCoordinatorForDrain is the other half: a genuinely different
// PROCESS holds the OS session lock.
type lockBusyCoordinatorForDrain struct {
	calls atomic.Int64
}

func (c *lockBusyCoordinatorForDrain) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	c.calls.Add(1)
	return nil, &session.SessionLockBusyError{Path: "/tmp/probe.lock", HolderPID: 4242}
}

// TestDrainSessionNow_QueuedNotExecutedIsNotDrained proves the core P0-1
// contract for the in-process case.
//
// Revert-check: move `drained = true` back to immediately after the
// LeaseRunQueueEntry success and this fails on the require.False below —
// DrainSessionNow answers (true, nil), which is the false success itself.
func TestDrainSessionNow_QueuedNotExecutedIsNotDrained(t *testing.T) {
	t.Parallel()
	sess, svc := setupTestSession(t, "drain-busy-queued")
	coord := &busyCoordinatorForDrain{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-busy-queued",
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "busy-queued-idem-1", sess.ID, callData))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drained, err := pump.DrainSessionNow(ctx, sess.ID)

	require.NoError(t, err, "an externally-owned session is ordinary contention, not a failure of this call")
	require.False(t, drained,
		"nothing executed here — the call was appended to another owner's mailbox. Reporting drained=true makes RunNonInteractive turn a cancelled run into exit code 0 for work that has not run and will not run in this process")
	require.Equal(t, int64(1), coord.calls.Load(), "the coordinator must have been consulted exactly once")

	// The row must be back to pending with NO attempt charged, so the real
	// owner (or a later process) still gets to run it.
	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Len(t, pending, 1, "the entry must be released back to pending, not consumed")
	require.Equal(t, int64(0), pending[0].Attempts, "routine contention must never count as a failed attempt")
}

// TestDrainSessionNow_SessionLockBusyIsNotDrained is the cross-process half:
// another live crush process holds the OS session lock.
func TestDrainSessionNow_SessionLockBusyIsNotDrained(t *testing.T) {
	t.Parallel()
	sess, svc := setupTestSession(t, "drain-busy-lock")
	coord := &lockBusyCoordinatorForDrain{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-busy-lock",
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "busy-lock-idem-1", sess.ID, callData))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drained, err := pump.DrainSessionNow(ctx, sess.ID)

	require.NoError(t, err)
	require.False(t, drained,
		"another process holds the session lock — this call ran nothing and must not claim it did")
	require.Equal(t, int64(1), coord.calls.Load())

	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, int64(0), pending[0].Attempts, "lock contention must never count as a failed attempt")
}

// TestDrainSessionNow_ExecutedEntryStillReportsDrained is the control: the
// fix must not have made drained unreachable. A turn that genuinely runs and
// commits still has to report true, or RunNonInteractive would restore the
// stale cancellation over a continuation that DID complete — the opposite
// error, and the one this whole function exists to prevent.
func TestDrainSessionNow_ExecutedEntryStillReportsDrained(t *testing.T) {
	t.Parallel()
	sess, svc := setupTestSession(t, "drain-busy-control")
	coord := &countingCoordinatorForDrain{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       svc,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-busy-control",
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "busy-control-idem-1", sess.ID, callData))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drained, err := pump.DrainSessionNow(ctx, sess.ID)

	require.NoError(t, err)
	require.True(t, drained, "a committed turn must still report drained=true")
	require.Equal(t, int64(1), coord.calls.Load())
}
