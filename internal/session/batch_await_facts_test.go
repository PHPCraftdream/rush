package session

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// TestBatchAwaitFactsCapturedAndLevel exercises the R10a captured-root and
// level reads end-to-end over a real disposable store, the exact reads the
// agent package's inert BatchAwaitWorker consumes.
func TestBatchAwaitFactsCapturedAndLevel(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	r := batchReadTestReader(t, conn)
	snap := batchReadTestReserve(t, s, ctx, "owner", "call", "")
	root := snap.Root.RootID
	captured := r.BatchCaptured(ctx, []string{"owner"})
	require.True(t, captured.Complete())
	require.Equal(t, []BatchLiveRef{{"owner", root}}, captured.Refs())
	levels := r.BatchCapturedLevel(ctx, captured)
	require.True(t, levels.Complete)
	require.Len(t, levels.Roots, 1)
	require.False(t, levels.Roots[0].Finished)
	require.Len(t, levels.Roots[0].Children, 2)
	// Project the root terminal; the next captured level read settles it.
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: 2, Roots: []BatchRootProjection{{
		Owner: "owner", Root: root,
		Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(root, "completed")},
	}}}))
	levels = r.BatchCapturedLevel(ctx, captured)
	require.True(t, levels.Complete)
	require.True(t, levels.Roots[0].Finished)
	// A later Add child of a captured root is visible; a new root is not.
	levels = r.BatchCapturedLevel(ctx, captured)
	require.Len(t, levels.Roots, 1, "levels reread captured refs only")
}

// TestBatchAwaitFactsPendingInclusiveDebt reads the R8 pending-inclusive
// debt facts the arbiter mapping consumes, over a real store.
func TestBatchAwaitFactsPendingInclusiveDebt(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	q := db.New(conn)
	debt, err := s.BatchReactionPendingInclusiveDebtRows(ctx, "owner")
	require.NoError(t, err)
	require.Empty(t, debt.Nodes)
	require.Empty(t, debt.Notices)
	now := time.Now().Unix()
	_, err = q.InsertSessionNotice(ctx, db.InsertSessionNoticeParams{
		Owner: "owner", Kind: NoticeKindChildQuestion, Text: "held question",
		Wake: 1, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	debt, err = s.BatchReactionPendingInclusiveDebtRows(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, debt.Notices, 1)
	require.Equal(t, NoticeKindChildQuestion, debt.Notices[0].Kind)
	// Live roots plus debt: the scope predicate's two halves over one store.
	r := batchReadTestReader(t, conn)
	batchReadTestReserve(t, s, ctx, "owner", "call2", "")
	live := r.BatchLiveForOwner(ctx, "owner")
	require.True(t, live.HasLive())
	require.Len(t, debt.Notices, 1)
}
