package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBatchRecoverRootWithRunnerIDInterrupts: production roots always carry a
// runner id, but every other recovery fixture reserves with an empty one.
// AdvanceBatchRootSequence fences on runner_id, so a recovery that does not
// pass the root's runner id advances zero rows, surfaces as a projection
// conflict and rolls the whole root back -- dead-host recovery would never
// succeed for a real root.
// Revert-check: dropping RunnerID from the AdvanceBatchRootSequenceParams in
// batch_recovery.go makes BatchRecoverRoot return ErrBatchProjectionConflict
// and Interrupted stay zero.
func TestBatchRecoverRootWithRunnerIDInterrupts(t *testing.T) {
	a, _, conn, dir, ctx := r9Stores(t)
	p := batchStoreTestReservation()
	p.Host = "host"
	p.RunnerID = "runner-1"
	p.Root.Children = nil
	r, err := a.Reserve(ctx, p)
	require.NoError(t, err)
	root := r.Snapshot.Root.RootID
	r9Announce(t, conn, ctx, root)
	budgetDeadHost(t, dir, "host")

	out, err := a.BatchRecoverRoot(ctx, root, BatchSharedHostProbe(dir))
	require.NoError(t, err)
	require.EqualValues(t, 1, out.Interrupted)
	require.EqualValues(t, 1, out.Notices)

	got, err := a.Read(ctx, "owner", root)
	require.NoError(t, err)
	require.Equal(t, "interrupted", got.Root.State)
	require.Equal(t, "host_lost", got.Root.OutcomeCode)
	require.Zero(t, got.Root.Wake)
}
