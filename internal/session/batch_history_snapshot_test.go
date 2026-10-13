// Batch activity snapshot coverage (R10c chunk 3): structured per-root
// snapshots (nodes, counts, debt, delivery views) with the host verdict
// taken from the R10a reader's injectable probe, plus the owner root list
// and the audit state counts.
package session

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// Revert-check (mutation "snapshot host verdict not injectable / probe
// bypassed"): hardcoding the verdict, probing the filesystem directly or
// ignoring ProbeHost makes the Dead/Alive flip below fail.
func TestBatchHistorySnapshotShapeAndInjectableHostVerdict(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	m := message.NewService(db.New(conn))
	snap := batchStoreTestReserve(t, s, ctx)
	_, err := s.BatchAnnounceStarted(ctx, m, "owner", snap.Root.RootID, batchOutboxText("started"))
	require.NoError(t, err)
	batchOutboxSettle(t, s, ctx, snap, 2, snap.Nodes[1].NodeID, snap.Nodes[2].NodeID, snap.Root.RootID)
	r := batchReadTestReader(t, conn)
	verdict := HostStatusAlive
	r.ProbeHost = func(string, string) (HostLockStatus, error) { return verdict, nil }

	got, err := s.BatchHistorySnapshot(ctx, r, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.False(t, got.Disappeared)
	require.Len(t, got.Nodes, 3)
	require.Equal(t, snap.Root.RootID, got.Root.Fact.NodeID)
	require.Equal(t, HostStatusAlive, got.Host)
	require.Equal(t, 2, got.Counts.Completed, "tasks only; the structural root never counts")
	require.Equal(t, "pending", got.Nodes[1].Delivery)
	require.True(t, got.Nodes[1].Wake)
	require.False(t, got.Nodes[1].Reacted)
	require.Equal(t, snap.Nodes[1].NodeID, got.Nodes[1].Fact.NodeID)
	require.Equal(t, 3, int(got.Debt), "settled unreacted woken nodes are debt")
	// The settled notice has no message until pulled: no dead reference.
	require.Empty(t, got.Root.MessageID)

	verdict = HostStatusDead
	dead, err := s.BatchHistorySnapshot(ctx, r, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, HostStatusDead, dead.Host)
	require.Equal(t, HostStatusDead, dead.Nodes[1].Fact.HostStatus)
	verdict = HostStatusUnknown
	unknown, err := s.BatchHistorySnapshot(ctx, r, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, HostStatusUnknown, unknown.Host)
	require.False(t, unknown.Counts == BatchLiveCounts{} && unknown.Debt == 0 && len(unknown.Nodes) == 0, "an unknown host verdict never reads as an empty session")

	// Debt after reaction drops out of the snapshot.
	_, err = conn.ExecContext(ctx, `UPDATE batch_nodes SET reacted=1 WHERE owner_session_id='owner'`)
	require.NoError(t, err)
	reacted, err := s.BatchHistorySnapshot(ctx, r, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.Zero(t, reacted.Debt)
	for _, n := range reacted.Nodes {
		require.True(t, n.Reacted)
	}
}

func TestBatchHistorySnapshotDisappearedAndUnknownRoot(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	batchStoreTestReserve(t, s, ctx)
	r := batchReadTestReader(t, conn)
	ref := r.BatchCaptured(ctx, []string{"owner"}).Refs()[0]
	require.NoError(t, db.New(conn).DeleteSession(ctx, "owner"))
	got, err := s.BatchHistorySnapshot(ctx, r, ref.OwnerSessionID, ref.RootID)
	require.NoError(t, err)
	require.True(t, got.Disappeared)
	require.Empty(t, got.Nodes)
	_, err = s.BatchHistorySnapshot(ctx, r, "", "")
	require.Error(t, err)
	_, err = s.BatchHistorySnapshot(ctx, nil, "owner", "root")
	require.ErrorIs(t, err, errBatchReadUnavailable)
}

// Revert-check (mutation "root list drops terminal incarnations" via a
// filter regression on BatchHistoryRootsForOwner): the second, older,
// terminal incarnation below must stay in the list.
func TestBatchHistoryRootViewsOldestFirstAllIncarnations(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	first := batchReadTestReserve(t, s, ctx, "owner", "call-1", "")
	p := batchStoreTestReservation()
	p.Owner, p.ToolCallID = "owner", "call-2"
	second, err := s.Reserve(ctx, p)
	require.NoError(t, err)
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: 5, Roots: []BatchRootProjection{{Owner: "owner", Root: first.Root.RootID, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(first.Root.NodeID, "completed")}}}}))
	// BatchHistoryRootsForOwner orders by created_at,root_id; same-second
	// inserts tiebreak on a random uuid root_id, so pin the ordering.
	_, err = conn.ExecContext(ctx, `UPDATE batch_nodes SET created_at=100 WHERE root_id=?`, first.Root.RootID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `UPDATE batch_nodes SET created_at=200 WHERE root_id=?`, second.Snapshot.Root.RootID)
	require.NoError(t, err)
	r := batchReadTestReader(t, conn)
	views, err := s.BatchHistoryRootViews(ctx, r, "owner")
	require.NoError(t, err)
	require.Len(t, views, 2)
	require.Equal(t, first.Root.RootID, views[0].Fact.Ref.RootID)
	require.Equal(t, "completed", views[0].Fact.State, "terminal incarnations stay visible")
	require.Equal(t, "none", views[0].Delivery)
	require.Equal(t, second.Snapshot.Root.RootID, views[1].Fact.Ref.RootID)
	require.Equal(t, "call-2", views[1].Fact.ToolCallID)
	empty, err := s.BatchHistoryRootViews(ctx, r, "nobody")
	require.NoError(t, err)
	require.Empty(t, empty)
}

// sessions audit input: state counts over all incarnations, tasks only.
func TestBatchHistoryStateCounts(t *testing.T) {
	t.Parallel()
	s, conn, ctx := batchStoreTestDB(t)
	p := batchStoreTestReservation()
	p.Root.Children = []BatchReservationNode{batchStoreTestTask(), func() BatchReservationNode {
		n := batchStoreTestTask()
		n.Values.State = "failed"
		return n
	}()}
	snap, err := s.Reserve(ctx, p)
	require.NoError(t, err)
	require.NoError(t, s.Project(ctx, BatchProjection{Host: "host", Sequence: 2, Roots: []BatchRootProjection{{Owner: "owner", Root: snap.Snapshot.Root.RootID, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(snap.Snapshot.Nodes[1].NodeID, "completed")}}}}))
	counts, err := db.New(conn).BatchHistoryStateCounts(ctx, "owner")
	require.NoError(t, err)
	byState := map[string]int64{}
	for _, c := range counts {
		byState[c.State] = c.Count
	}
	require.EqualValues(t, 1, byState["completed"])
	require.EqualValues(t, 1, byState["failed"])
	require.NotContains(t, byState, "pending", "the structural batch root is not a task")
}
