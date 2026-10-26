package session

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestBatchReadHeldTerminationAndDeliveryIndependent(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	snap := batchReadTestReserve(t, s, ctx, "owner", "held", "")
	r := batchReadTestReader(t, conn)
	captured := r.BatchCaptured(ctx, []string{"owner"})
	ref := captured.Refs()[0]
	// Stop intent and a successful control answer do not report termination.
	root := batchStoreTestUpdate(ref.RootID, "cancelling")
	root.Cause = "stop"
	root.ControlState = "acked"
	leaf := batchStoreTestUpdate(ref.RootID+".1", "cancelling")
	leaf.Cause = "stop"
	leaf.ControlState = "acked"
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: 2, Roots: []BatchRootProjection{{Owner: "owner", Root: ref.RootID, Updates: []db.ProjectBatchNodeParams{root, leaf}}}}))
	held := r.BatchCapturedLevel(ctx, captured)
	require.True(t, held.Complete)
	require.True(t, held.HasLive())
	require.False(t, held.Roots[0].Finished)
	// Committed settlement is observable before announcement/history delivery.
	batchOutboxSettle(t, s, ctx, snap, 3, ref.RootID, ref.RootID+".1", ref.RootID+".2")
	finished := r.BatchCapturedLevel(ctx, captured)
	require.True(t, finished.Complete)
	require.True(t, finished.Roots[0].Finished)
	require.False(t, finished.HasLive())
	require.Equal(t, 2, finished.Roots[0].Counts.Completed, "two task nodes, not the structural root")
	stored, err := s.Read(ctx, "owner", ref.RootID)
	require.NoError(t, err)
	require.EqualValues(t, 0, stored.Root.Announced)
	require.Equal(t, "pending", stored.Root.Delivery)
	require.False(t, r.BatchLiveForOwner(ctx, "owner").HasLive())
	m := message.NewService(db.New(conn))
	pulled, err := s.BatchPullNotices(ctx, m, "owner", batchOutboxParams)
	require.NoError(t, err)
	require.Empty(t, pulled, "root acknowledgement still gates delivery")
}

func TestBatchReadRealSharedHostProbeAndTerminalLinks(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	require.NoError(t, seedSession(ctx, db.New(conn), "child"))
	snap := batchReadTestReserve(t, s, ctx, "owner", "delegate", "child")
	batchReadTestReserve(t, s, ctx, "child", "own", "")
	dir := t.TempDir()
	r := NewBatchLiveReader(db.New(conn), dir)
	holder, err := TryAcquireFileLock(HostLockPath(dir, "host"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Release() })
	live := r.BatchLiveForOwner(ctx, "owner")
	require.True(t, live.Complete)
	require.Equal(t, HostStatusAlive, live.Own[0].HostStatus)
	require.NoError(t, holder.Release())
	live = r.BatchLiveForOwner(ctx, "owner")
	require.True(t, live.Complete)
	require.True(t, live.HasLive())
	require.Equal(t, HostStatusDead, live.Own[0].HostStatus)
	// A reader must not hold an exclusive lock after the shared dead-host probe.
	later, err := TryAcquireFileLock(HostLockPath(dir, "host"))
	require.NoError(t, err)
	require.NoError(t, later.Release())
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: 2, Roots: []BatchRootProjection{{Owner: "owner", Root: snap.Root.RootID, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(snap.Root.RootID, "completed")}}}}))
	closed := r.BatchLiveForOwner(ctx, "owner")
	require.True(t, closed.Complete)
	require.Empty(t, closed.Descendants)
	require.Empty(t, closed.Tree)
	require.True(t, r.BatchLiveForOwner(ctx, "child").HasLive(), "terminal delegation root no longer traverses independent child work")
}

func TestBatchReadOwnerCascadePreservesCapturedUncertainty(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	batchReadTestReserve(t, s, ctx, "owner", "cascade", "")
	r := batchReadTestReader(t, conn)
	captured := r.BatchCaptured(ctx, []string{"owner"})
	require.True(t, captured.Complete())
	require.Len(t, captured.Refs(), 1)
	require.NoError(t, db.New(conn).DeleteSession(ctx, "owner"))
	levels := r.BatchCapturedLevel(ctx, captured)
	require.True(t, levels.Complete)
	require.Len(t, levels.Roots, 1)
	require.Equal(t, captured.Refs()[0], levels.Roots[0].Ref)
	require.True(t, levels.Roots[0].Disappeared)
	require.False(t, levels.Roots[0].Finished)
	require.True(t, levels.Roots[0].HasLive())
	require.True(t, levels.HasLive())
	require.Empty(t, levels.Roots[0].Subtree)
	require.False(t, r.BatchLiveForOwner(ctx, "owner").HasLive())
}

func TestBatchReadLevelCountsEveryTaskState(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	p := batchStoreTestReservation()
	p.Root.Children = nil
	states := []string{"pending", "starting", "running", "cancelling", "completed", "failed", "cancelled", "timed_out", "interrupted", "skipped"}
	for _, state := range states {
		n := batchStoreTestTask()
		n.Values.State = state
		p.Root.Children = append(p.Root.Children, n)
	}
	snap, err := s.Reserve(ctx, p)
	require.NoError(t, err)
	r := batchReadTestReader(t, conn)
	level := r.BatchLevel(ctx, BatchLiveRef{"owner", snap.Snapshot.Root.RootID}, "")
	require.True(t, level.Complete)
	require.Equal(t, BatchLiveCounts{Pending: 1, Starting: 1, Running: 1, Cancelling: 1, Active: 3, Completed: 1, Failed: 1, Cancelled: 1, TimedOut: 1, Interrupted: 1, Skipped: 1}, level.Counts)
	require.Equal(t, level.Counts, level.Node.Counts)
	require.Equal(t, level.Counts, r.BatchLiveForOwner(ctx, "owner").Counts)
	require.Equal(t, BatchLiveCounts{Starting: 1, Active: 1}, level.Children[1].Counts)
	require.Equal(t, BatchLiveCounts{Running: 1, Active: 1}, level.Children[2].Counts)
	require.Equal(t, BatchLiveCounts{Cancelling: 1, Active: 1}, level.Children[3].Counts)
	empty := r.BatchCaptured(ctx, []string{"idle"})
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.True(t, r.BatchCapturedLevel(canceled, empty).HasLive())
	var missing *BatchLiveReader
	require.True(t, missing.BatchCapturedLevel(ctx, empty).HasLive())
}
