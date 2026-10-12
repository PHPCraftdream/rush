package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

type batchReadTestSource struct {
	BatchLiveSource
	calls, maxChunk int
	failOwner       string
	failLevel       bool
}

func (s *batchReadTestSource) BatchLiveNodesForOwners(ctx context.Context, owners []string) ([]db.BatchNode, error) {
	s.calls++
	s.maxChunk = max(s.maxChunk, len(owners))
	for _, owner := range owners {
		if owner == s.failOwner {
			return nil, errors.New("injected owner read failure")
		}
	}
	return s.BatchLiveSource.BatchLiveNodesForOwners(ctx, owners)
}

func (s *batchReadTestSource) BatchLevelNodes(ctx context.Context, p db.BatchLevelNodesParams) ([]db.BatchNode, error) {
	if s.failLevel {
		return nil, errors.New("injected level failure")
	}
	return s.BatchLiveSource.BatchLevelNodes(ctx, p)
}

func batchReadTestReader(t *testing.T, conn *sql.DB) *BatchLiveReader {
	t.Helper()
	r := NewBatchLiveReader(db.New(conn), t.TempDir())
	r.IsOwnHost = func(string) bool { return false }
	r.ProbeHost = func(string, string) (HostLockStatus, error) { return HostStatusAlive, nil }
	return r
}

func batchReadTestReserve(t *testing.T, s *BatchStore, ctx context.Context, owner, call, child string) BatchSnapshot {
	t.Helper()
	p := batchStoreTestReservation()
	p.Owner = owner
	p.ToolCallID = call
	p.Root.Children[0].Values.ChildSessionID = batchStoreTestString(child)
	r, err := s.Reserve(ctx, p)
	require.NoError(t, err)
	return r.Snapshot
}

func TestBatchReadStatesAndHostFacts(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	r := batchReadTestReader(t, conn)
	snap := batchReadTestReserve(t, s, ctx, "owner", "call", "")
	ref := BatchLiveRef{"owner", snap.Root.RootID}
	states := []string{"pending", "starting", "running", "cancelling", "completed", "failed", "cancelled", "timed_out", "interrupted", "skipped"}
	for i, state := range states {
		p := BatchRootProjection{Owner: "owner", Root: ref.RootID, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(ref.RootID, state)}}
		require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: int64(i + 2), Roots: []BatchRootProjection{p}}))
		live := r.BatchLiveForOwner(ctx, "owner")
		require.True(t, live.Complete, state)
		require.Equal(t, i < 4, live.HasLive(), state)
		level := r.BatchLevel(ctx, ref, "")
		require.True(t, level.Complete, state)
		require.Equal(t, i >= 4, level.Finished, state)
	}
	// Return to a nonterminal projection for independent host facts.
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: 20, Roots: []BatchRootProjection{{Owner: "owner", Root: ref.RootID, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(ref.RootID, "cancelling")}}}}))
	for _, st := range []HostLockStatus{HostStatusAlive, HostStatusDead, HostStatusUnknown} {
		probes := 0
		r.ProbeHost = func(string, string) (HostLockStatus, error) { probes++; return st, nil }
		live := r.BatchLiveForOwner(ctx, "owner")
		require.True(t, live.HasLive())
		require.Equal(t, st != HostStatusUnknown, live.Complete)
		require.Equal(t, st, live.Own[0].HostStatus)
		require.Equal(t, 1, probes, "one shared host probe across root and tasks")
	}
	r.ProbeHost = func(string, string) (HostLockStatus, error) { return HostStatusDead, errors.New("probe failed") }
	require.False(t, r.BatchLiveForOwner(ctx, "owner").Complete)
	r.IsOwnHost = func(string) bool { return true }
	r.ProbeHost = func(string, string) (HostLockStatus, error) {
		t.Fatal("own host probed")
		return HostStatusUnknown, nil
	}
	require.True(t, r.BatchLiveForOwner(ctx, "owner").Complete)
}

func TestBatchReadScopeDistinctionsCyclesAndChunks(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	q := db.New(conn)
	for _, owner := range []string{"child", "grandchild", "unlinked"} {
		require.NoError(t, seedSession(ctx, q, owner))
	}
	a := batchReadTestReserve(t, s, ctx, "owner", "a", "child")
	batchReadTestReserve(t, s, ctx, "child", "b", "grandchild")
	batchReadTestReserve(t, s, ctx, "grandchild", "c", "owner")
	batchReadTestReserve(t, s, ctx, "unlinked", "d", "")
	// A settled task link inside a still-live root remains scope evidence.
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: 2, Roots: []BatchRootProjection{{Owner: "owner", Root: a.Root.RootID, Updates: []db.ProjectBatchNodeParams{func() db.ProjectBatchNodeParams {
		v := batchStoreTestUpdate(a.Root.RootID+".1", "completed")
		v.ChildSessionID = batchStoreTestString("child")
		return v
	}()}}}}))
	r := batchReadTestReader(t, conn)
	source := &batchReadTestSource{BatchLiveSource: q}
	r.Source = source
	probes := 0
	r.ProbeHost = func(string, string) (HostLockStatus, error) { probes++; return HostStatusDead, nil }
	got := r.BatchLiveForSessions(ctx, []string{"owner", "child", "idle", "owner", ""})
	require.Len(t, got, 3)
	require.Len(t, got["owner"].Own, 1)
	require.Len(t, got["owner"].Tree, 3)
	require.Len(t, got["owner"].Descendants, 2)
	require.Equal(t, 5, got["owner"].Counts.Pending)
	require.Equal(t, 1, got["owner"].Counts.Completed)
	require.Equal(t, 1, probes)
	require.LessOrEqual(t, source.calls, 3, "shared forest fetch scales with depth")
	require.Len(t, r.BatchCaptured(ctx, []string{"owner"}).Refs(), 3)
	require.False(t, got["idle"].HasLive())
	for _, node := range got["owner"].Descendants {
		require.NotEqual(t, "owner", node.ChildSessionID)
	}
	for _, owner := range []string{"owner", "child", "idle"} {
		single := r.BatchLiveForOwner(ctx, owner)
		require.Equal(t, single, got[owner])
	}
	source.failOwner = "grandchild"
	partial := r.BatchLiveForOwner(ctx, "owner")
	require.True(t, partial.OwnComplete)
	require.False(t, partial.Complete)
	require.True(t, partial.HasLiveDescendants())
	source.failOwner = ""
	source.calls = 0
	roots := make([]string, 1100)
	for i := range roots {
		roots[i] = fmt.Sprintf("idle-%d", i)
	}
	all := r.BatchLiveForSessions(ctx, roots)
	require.Len(t, all, 1100)
	require.Equal(t, 3, source.calls)
	require.Equal(t, 500, source.maxChunk)
}

func TestBatchReadCaptureAddDisappearAndIdentity(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	snap := batchReadTestReserve(t, s, ctx, "owner", "a", "")
	r := batchReadTestReader(t, conn)
	captured := r.BatchCaptured(ctx, []string{"owner", "owner"})
	require.True(t, captured.Complete())
	require.Len(t, captured.Refs(), 1)
	refs := captured.Refs()
	refs[0].RootID = "tampered"
	ref := captured.Refs()[0]
	require.Equal(t, snap.Root.RootID, ref.RootID)
	nested := batchStoreTestReservation().Root
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: 2, Roots: []BatchRootProjection{{Owner: "owner", Root: ref.RootID, Additions: []BatchAddition{{Parent: ref.RootID, Ordinal: 10, Node: nested}, {Parent: ref.RootID, Ordinal: 3, Node: batchStoreTestTask()}}}}}))
	batchReadTestReserve(t, s, ctx, "owner", "late", "")
	levels := r.BatchCapturedLevel(ctx, captured)
	require.True(t, levels.Complete)
	require.Len(t, levels.Roots, 1)
	level := levels.Roots[0]
	require.Len(t, level.Children, 4)
	require.Equal(t, []int64{1, 2, 3, 10}, []int64{level.Children[0].Ordinal, level.Children[1].Ordinal, level.Children[2].Ordinal, level.Children[3].Ordinal})
	require.Equal(t, 5, level.Counts.Pending)
	require.Equal(t, 2, level.Children[3].Counts.Pending)
	require.Len(t, level.Subtree, 7)
	nestedLevel := r.BatchLevel(ctx, ref, ref.RootID+".10")
	require.Len(t, nestedLevel.Children, 2)
	require.Equal(t, 2, nestedLevel.Counts.Pending)
	wrong := r.BatchLevel(ctx, BatchLiveRef{"other", ref.RootID}, "")
	require.True(t, wrong.Disappeared)
	require.False(t, wrong.Finished)
	require.True(t, wrong.HasLive())
	require.NoError(t, s.DeleteRoot(ctx, "owner", "host", ref.RootID))
	batchReadTestReserve(t, s, ctx, "owner", "a", "")
	gone := r.BatchCapturedLevel(ctx, captured)
	require.True(t, gone.Roots[0].Disappeared)
	require.False(t, gone.Roots[0].Finished)
	require.True(t, gone.HasLive())
}

func TestBatchReadConservativeFailuresAndZeroValues(t *testing.T) {
	t.Parallel()
	require.True(t, (BatchLiveResult{}).HasLive())
	require.True(t, (BatchLiveResult{}).HasLiveOwn())
	require.True(t, (BatchLiveResult{}).HasLiveDescendants())
	require.True(t, (BatchLevelResult{}).HasLive())
	require.True(t, (BatchCapturedRoots{}).HasLive())
	require.True(t, (BatchCapturedLevels{}).HasLive())
	var nilReader *BatchLiveReader
	require.True(t, nilReader.BatchLiveForOwner(context.Background(), "owner").HasLive())
	require.True(t, nilReader.BatchLevel(context.Background(), BatchLiveRef{"owner", "root"}, "").HasLive())
	s, conn, ctx := batchStoreTestDB(t)
	snap := batchReadTestReserve(t, s, ctx, "owner", "a", "")
	r := batchReadTestReader(t, conn)
	source := &batchReadTestSource{BatchLiveSource: db.New(conn), failOwner: "owner"}
	r.Source = source
	captured := r.BatchCaptured(ctx, []string{"owner"})
	require.False(t, captured.Complete())
	require.True(t, r.BatchCapturedLevel(ctx, captured).HasLive())
	source.failOwner = ""
	source.failLevel = true
	require.False(t, r.BatchLevel(ctx, BatchLiveRef{"owner", snap.Root.RootID}, "").Complete)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	require.False(t, r.BatchLiveForOwner(canceled, "owner").Complete)
	// A genuinely empty, complete scope can prove absence.
	empty := r.BatchCaptured(ctx, []string{"idle"})
	require.True(t, empty.Complete())
	require.False(t, r.BatchCapturedLevel(ctx, empty).HasLive())
	// Reader source failure must not silently fall back to BatchStore's writer.
	other, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	r.Source = db.New(other)
	require.False(t, r.BatchLiveForOwner(ctx, "owner").Complete)
	require.False(t, r.BatchLevel(ctx, BatchLiveRef{"owner", snap.Root.RootID}, "").Complete)
}

func TestBatchReadChunkBoundariesAndSelectiveFailure(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	owners := make([]string, 501)
	for i := range owners {
		owners[i] = fmt.Sprintf("boundary-%d", i)
	}
	// Real roots on both sides of the boundary pin fact retention as well as
	// completeness; other owners intentionally have no work.
	for _, i := range []int{0, 499, 500} {
		require.NoError(t, seedSession(ctx, db.New(conn), owners[i]))
		batchReadTestReserve(t, s, ctx, owners[i], "call", "")
	}
	r := batchReadTestReader(t, conn)
	for _, size := range []int{499, 500, 501} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			source := &batchReadTestSource{BatchLiveSource: db.New(conn)}
			r.Source = source
			got := r.BatchLiveForSessions(ctx, owners[:size])
			require.Len(t, got, size)
			require.Equal(t, (size+499)/500, source.calls)
			require.Equal(t, min(size, 500), source.maxChunk)
			for i, owner := range owners[:size] {
				require.True(t, got[owner].Complete, owner)
				require.True(t, got[owner].OwnComplete, owner)
				require.Equal(t, i == 0 || i == 499 || i == 500, got[owner].HasLive(), owner)
			}
		})
	}
	// Only the second chunk fails. The first chunk must retain complete live
	// and complete empty answers, not inherit another chunk's uncertainty.
	source := &batchReadTestSource{BatchLiveSource: db.New(conn), failOwner: owners[500]}
	r.Source = source
	got := r.BatchLiveForSessions(ctx, owners)
	require.Equal(t, 2, source.calls)
	for i, owner := range owners {
		read := got[owner]
		if i < 500 {
			require.True(t, read.Complete, owner)
			require.True(t, read.OwnComplete, owner)
			require.NoError(t, read.Err)
			require.Equal(t, i == 0 || i == 499, read.HasLive(), owner)
		} else {
			require.False(t, read.Complete)
			require.False(t, read.OwnComplete)
			require.Error(t, read.Err)
			require.Empty(t, read.Tree)
			require.True(t, read.HasLive())
			require.True(t, read.HasLiveOwn())
			require.True(t, read.HasLiveDescendants())
		}
	}
}

func TestBatchReadDepthTruncation(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	for i := 0; i < batchReadDepth+2; i++ {
		owner := fmt.Sprintf("s%d", i)
		require.NoError(t, seedSession(ctx, db.New(conn), owner))
		child := fmt.Sprintf("s%d", i+1)
		batchReadTestReserve(t, s, ctx, owner, "call", child)
	}
	r := batchReadTestReader(t, conn)
	live := r.BatchLiveForOwner(ctx, "s0")
	require.True(t, live.OwnComplete)
	require.False(t, live.Complete)
	require.True(t, live.HasLive())
	require.Len(t, live.Tree, batchReadDepth+1)
}
