package session_test

// Regression coverage for P0-3 of the 2026-08-18 release-readiness review
// (task #552): a failing Ack after a successful turn was logged and then
// reported as a terminal success.
//
// The failure is invisible to every other test in this package because they
// all use a healthy session service, where Ack never fails. It needs a
// service that fails exactly one call and nothing else.

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// ackFailingService wraps a real session.Service and fails AckRunQueueEntry.
// Everything else — leasing, renewal, listing, nacking — goes to the real
// implementation, so the entry genuinely reaches the ack path rather than
// falling over somewhere earlier and passing this test for the wrong reason.
type ackFailingService struct {
	session.Service
	ackCalls atomic.Int64
	ackErr   error
}

func (s *ackFailingService) AckRunQueueEntry(ctx context.Context, id, leasedBy string) (string, error) {
	s.ackCalls.Add(1)
	return "", s.ackErr
}

// TestDrainSessionNow_AckFailureIsNotReportedAsSuccess proves the whole
// chain: executeEntrySync must return ErrTurnCommitFailed rather than nil,
// and DrainSessionNow must surface it instead of telling its caller the
// durable continuation completed.
//
// Revert-check: change the ack branch back to logging and `return nil` and
// this fails on the require.Error below — DrainSessionNow returns (true, nil),
// exactly the false success the review found.
func TestDrainSessionNow_AckFailureIsNotReportedAsSuccess(t *testing.T) {
	t.Parallel()
	sess, svc := setupTestSession(t, "drain-ack-failure")
	failing := &ackFailingService{Service: svc, ackErr: errors.New("disk on fire")}
	coord := &countingCoordinatorForDrain{}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       failing,
		Coordinator:    coord,
		PumpInstanceID: "test-pump-ack-failure",
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "ack-failure-idem-1", sess.ID, callData))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	drained, err := pump.DrainSessionNow(ctx, sess.ID)

	require.Error(t, err, "a turn whose Ack failed was never committed — reporting success would tell the operator work landed that did not")
	require.ErrorIs(t, err, session.ErrTurnCommitFailed)
	require.ErrorContains(t, err, "disk on fire", "the underlying cause must survive wrapping, or the log is the only place it exists")
	require.True(t, drained, "the turn DID execute — drained stays true; it is the commit that failed, not the run")

	require.Equal(t, int64(1), coord.calls.Load(), "the turn must have run exactly once")
	require.Equal(t, int64(1), failing.ackCalls.Load(), "the ack must have been attempted exactly once — a failed commit must not be retried by re-running the turn")
}

// TestDrainSessionNow_AckFailureLeavesTheRowRecoverable pins the deliberate
// choice not to nack a failed commit: the row stays leased so it recovers
// through the ordinary lease-expiry path, which charges an attempt and
// therefore eventually dead-letters. Nacking would return it to pending
// immediately and re-run a turn whose side effects already landed.
func TestDrainSessionNow_AckFailureLeavesTheRowRecoverable(t *testing.T) {
	t.Parallel()
	sess, svc := setupTestSession(t, "drain-ack-failure-row")
	failing := &ackFailingService{Service: svc, ackErr: errors.New("disk on fire")}
	pump := session.NewRunQueuePump(session.RunQueuePumpConfig{
		Sessions:       failing,
		Coordinator:    &countingCoordinatorForDrain{},
		PumpInstanceID: "test-pump-ack-failure-row",
	})

	callData, err := json.Marshal(map[string]any{"SessionID": sess.ID, "Prompt": "continue"})
	require.NoError(t, err)
	require.NoError(t, svc.EnqueueRunQueueEntry(context.Background(), "ack-failure-row-idem-1", sess.ID, callData))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = pump.DrainSessionNow(ctx, sess.ID)
	require.ErrorIs(t, err, session.ErrTurnCommitFailed)

	// Not pending: an immediate nack would make it so, and would re-run the
	// turn on the very next tick.
	pending, err := svc.ListPendingRunQueueEntries(context.Background())
	require.NoError(t, err)
	require.Empty(t, pending, "a failed commit must not be nacked back to pending — that re-runs an already-executed turn immediately")

	// Still there, still leased — recovery is the lease-expiry path's job.
	stale, err := svc.ListStaleLeasedRunQueueEntries(context.Background(), time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)
	require.Len(t, stale, 1, "the row must still exist and still be leased, so lease expiry can recover it with an attempt charged")
}
