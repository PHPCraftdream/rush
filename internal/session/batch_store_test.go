package session

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func batchStoreTestDB(t *testing.T) (*BatchStore, *sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	require.NoError(t, seedSession(ctx, db.New(conn), "owner"))
	return NewBatchStore(conn), conn, ctx
}
func batchStoreTestString(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
func batchStoreTestTask() BatchReservationNode {
	return BatchReservationNode{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: batchStoreTestString("test"), State: "pending", ControlState: "none"}}
}

func batchStoreTestReservation() BatchReservation {
	return BatchReservation{Owner: "owner", Host: "host", ToolCallID: "call", InputHash: "hash", Sequence: 1, Root: BatchReservationNode{Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: batchStoreTestString("parallel"), State: "pending", ControlState: "none"}, Children: []BatchReservationNode{batchStoreTestTask(), batchStoreTestTask()}}}
}

func batchStoreTestReserve(t *testing.T, s *BatchStore, ctx context.Context) BatchSnapshot {
	t.Helper()
	r, err := s.Reserve(ctx, batchStoreTestReservation())
	require.NoError(t, err)
	return r.Snapshot
}

func batchStoreTestUpdate(id, state string) db.ProjectBatchNodeParams {
	return db.ProjectBatchNodeParams{NodeID: id, State: state, ControlState: "none"}
}

func batchStoreTestNotice(id string) db.SelectBatchNodeNoticeParams {
	return db.SelectBatchNodeNoticeParams{NodeID: id, NoticeKind: batchStoreTestString("task"), NoticeState: batchStoreTestString("completed"), NoticeCause: batchStoreTestString(""), NoticeOrdinal: sql.NullInt64{Int64: 0, Valid: true}, NoticeCompleted: 1, Wake: 1}
}

func TestBatchReservationLateDBFailureIsAtomic(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	p := batchStoreTestReservation()
	p.Root.Children[1].Values.State = "invalid"
	_, err := s.Reserve(ctx, p)
	require.ErrorContains(t, err, "CHECK constraint")
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM batch_nodes").Scan(&count))
	require.Zero(t, count)
	batchStoreTestReserve(t, s, ctx)
}

func TestBatchLiveAttachMismatchAndUniqueRoot(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	first := batchStoreTestReserve(t, s, ctx)
	_, err := uuid.Parse(first.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, first.Root.RootID+".1", first.Nodes[1].NodeID)
	var wg sync.WaitGroup
	results := make(chan BatchReservationResult, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r, e := s.Reserve(ctx, batchStoreTestReservation()); results <- r; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	for r := range results {
		require.True(t, r.Attached)
		require.Equal(t, first.Root.RootID, r.Snapshot.Root.RootID)
	}
	p := batchStoreTestReservation()
	p.InputHash = "different"
	_, err = s.Reserve(ctx, p)
	require.ErrorIs(t, err, ErrBatchInputMismatch)
	for _, state := range []string{"pending", "starting", "running", "cancelling"} {
		_, err = conn.ExecContext(ctx, "UPDATE batch_nodes SET state=? WHERE node_id=?", state, first.Root.NodeID)
		require.NoError(t, err)
		r, e := s.Reserve(ctx, batchStoreTestReservation())
		require.NoError(t, e)
		require.True(t, r.Attached)
	}
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM batch_nodes WHERE node_id=root_id").Scan(&count))
	require.Equal(t, 1, count)
	// Bypass attachment to pin the database's unique-live index.
	_, err = conn.ExecContext(ctx, `INSERT INTO batch_nodes (node_id,root_id,owner_session_id,host_id,ordinal,kind,tool_call_id,input_hash,mode,state,created_at,updated_at) VALUES ('other','other','owner','host',0,'batch','call','hash','parallel','pending',1,1)`)
	require.Error(t, err)
	_, err = conn.ExecContext(ctx, "UPDATE batch_nodes SET state='completed' WHERE node_id=?", first.Root.NodeID)
	require.NoError(t, err)
	fresh, e := s.Reserve(ctx, batchStoreTestReservation())
	require.NoError(t, e)
	require.False(t, fresh.Attached)
	require.NotEqual(t, first.Root.RootID, fresh.Snapshot.Root.RootID)
}

func TestBatchTenStatesRoundTrip(t *testing.T) {
	s, _, ctx := batchStoreTestDB(t)
	snap := batchStoreTestReserve(t, s, ctx)
	for i, state := range []string{"pending", "starting", "running", "cancelling", "completed", "failed", "cancelled", "timed_out", "interrupted", "skipped"} {
		seq := int64(i + 1)
		p := BatchProjection{Host: "host", Sequence: seq + 1, Roots: []BatchRootProjection{{Owner: "owner", Root: snap.Root.RootID, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(snap.Root.NodeID, state)}}}}
		require.NoError(t, s.Project(ctx, p))
		got, e := s.Read(ctx, "owner", snap.Root.RootID)
		require.NoError(t, e)
		require.Equal(t, state, got.Root.State)
		require.Equal(t, seq+1, got.Root.RootSequence)
		require.Equal(t, got.Root, got.Nodes[0])
	}
	before, err := s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	p := BatchProjection{Host: "host", Sequence: 12, Roots: []BatchRootProjection{{Owner: "owner", Root: snap.Root.RootID, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(snap.Root.NodeID, "unknown")}}}}
	require.ErrorContains(t, s.Project(ctx, p), "CHECK constraint")
	after, err := s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestBatchProjectionLateFailureRollsBackUpdatesSequenceAndOutbox(t *testing.T) {
	s, _, ctx := batchStoreTestDB(t)
	snap := batchStoreTestReserve(t, s, ctx)
	leaf := snap.Nodes[1].NodeID
	p := BatchProjection{Host: "host", Sequence: 9, Roots: []BatchRootProjection{{Owner: "owner", Root: snap.Root.RootID, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(leaf, "completed")}, Notices: []db.SelectBatchNodeNoticeParams{batchStoreTestNotice(leaf), batchStoreTestNotice("missing")}}}}
	require.ErrorIs(t, s.Project(ctx, p), ErrBatchProjectionConflict)
	got, e := s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, e)
	require.Equal(t, snap, got)
	p.Roots[0].Notices = nil
	p.Roots[0].Updates = append(p.Roots[0].Updates, batchStoreTestUpdate("missing", "completed"))
	require.ErrorIs(t, s.Project(ctx, p), ErrBatchProjectionConflict)
	got, e = s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, e)
	require.Equal(t, snap, got)
	p.Roots[0].Updates = p.Roots[0].Updates[:1]
	p.Roots[0].Notices = []db.SelectBatchNodeNoticeParams{batchStoreTestNotice(leaf)}
	require.NoError(t, s.Project(ctx, p))
	got, e = s.Read(ctx, "owner", snap.Root.RootID)
	require.NoError(t, e)
	require.Equal(t, int64(9), got.Root.RootSequence)
	require.Equal(t, int64(1), got.Nodes[1].NoticeSelected)
	require.Equal(t, "pending", got.Nodes[1].Delivery)
	require.Zero(t, got.Root.Announced)
	require.ErrorIs(t, s.Project(ctx, p), ErrBatchProjectionConflict)
}

func TestBatchAddReservationsAndStaleIdentity(t *testing.T) {
	s, _, ctx := batchStoreTestDB(t)
	snap := batchStoreTestReserve(t, s, ctx)
	p := BatchProjection{Host: "wrong", Sequence: 4, Roots: []BatchRootProjection{{Owner: "owner", Root: snap.Root.RootID, Additions: []BatchAddition{{Parent: snap.Root.NodeID, Ordinal: 3, Node: batchStoreTestTask()}}}}}
	require.ErrorIs(t, s.Project(ctx, p), ErrBatchProjectionConflict)
	p.Host = "host"
	p.Roots[0].Root = "old-root"
	require.ErrorIs(t, s.Project(ctx, p), ErrBatchProjectionConflict)
	p.Roots[0].Root = snap.Root.RootID
	p.Roots[0].Additions = append(p.Roots[0].Additions, BatchAddition{Parent: snap.Root.NodeID, Ordinal: 4, Node: batchStoreTestTask()})
	p.Roots[0].Additions[1].Node.Values.State = "invalid"
	require.Error(t, s.Project(ctx, p))
	got, e := s.Read(ctx, "owner", p.Roots[0].Root)
	require.NoError(t, e)
	require.Equal(t, snap, got)
	p.Roots[0].Additions = p.Roots[0].Additions[:1]
	require.NoError(t, s.Project(ctx, p))
	got, e = s.Read(ctx, "owner", p.Roots[0].Root)
	require.NoError(t, e)
	require.Len(t, got.Nodes, 4)
	require.Equal(t, p.Roots[0].Root+".3", got.Nodes[3].NodeID)
	live, e := s.LiveRootsForHost(ctx, "host")
	require.NoError(t, e)
	require.Len(t, live, 1)
	live, e = s.LiveRootsForHost(ctx, "wrong")
	require.NoError(t, e)
	require.Empty(t, live)
}

func batchStoreTestCrossRoot(t *testing.T) (*BatchStore, context.Context, BatchSnapshot, BatchSnapshot, BatchProjection) {
	t.Helper()
	s, conn, ctx := batchStoreTestDB(t)
	require.NoError(t, seedSession(ctx, db.New(conn), "other-owner"))
	a := batchStoreTestReserve(t, s, ctx)
	r := batchStoreTestReservation()
	r.Owner, r.Sequence = "other-owner", 4
	reserved, err := s.Reserve(ctx, r)
	require.NoError(t, err)
	b := reserved.Snapshot
	p := BatchProjection{Host: "host", Sequence: 9, Roots: []BatchRootProjection{
		{Owner: "owner", Root: a.Root.RootID, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(a.Root.NodeID, "completed"), batchStoreTestUpdate(a.Nodes[1].NodeID, "completed"), batchStoreTestUpdate(a.Nodes[2].NodeID, "completed")}, Notices: []db.SelectBatchNodeNoticeParams{batchStoreTestNotice(a.Nodes[1].NodeID)}},
		{Owner: "other-owner", Root: b.Root.RootID, Updates: []db.ProjectBatchNodeParams{batchStoreTestUpdate(b.Root.NodeID, "running"), batchStoreTestUpdate(b.Nodes[1].NodeID, "starting")}},
	}}
	p.Roots[1].Updates[1].Token, p.Roots[1].Updates[1].Attempts = 1, 1
	return s, ctx, a, b, p
}

func TestBatchCrossRootSettleAndAdmit(t *testing.T) {
	s, ctx, a, b, p := batchStoreTestCrossRoot(t)
	require.NoError(t, s.Project(ctx, p))
	gotA, err := s.Read(ctx, "owner", a.Root.RootID)
	require.NoError(t, err)
	gotB, err := s.Read(ctx, "other-owner", b.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, "completed", gotA.Root.State)
	for _, node := range gotA.Nodes {
		require.Equal(t, "completed", node.State)
		require.Equal(t, int64(9), node.ProjectionSequence)
	}
	require.Equal(t, int64(9), gotA.Root.RootSequence)
	require.Equal(t, int64(1), gotA.Nodes[1].NoticeSelected)
	require.Equal(t, int64(9), gotA.Nodes[1].NoticeSequence.Int64)
	require.Equal(t, "pending", gotA.Nodes[1].Delivery)
	require.Equal(t, int64(1), gotA.Nodes[1].Wake)
	require.Equal(t, "running", gotB.Root.State)
	require.Equal(t, "starting", gotB.Nodes[1].State)
	require.Equal(t, int64(1), gotB.Nodes[1].Token)
	require.Equal(t, int64(1), gotB.Nodes[1].Attempts)
	require.Equal(t, int64(9), gotB.Root.RootSequence)
	require.Equal(t, int64(9), gotB.Nodes[1].ProjectionSequence)
	require.Equal(t, b.Nodes[2], gotB.Nodes[2])
	// A transition can next touch only B, skipping unrelated engine sequences.
	p.Sequence, p.Roots = 15, p.Roots[1:]
	p.Roots[0].Updates = []db.ProjectBatchNodeParams{batchStoreTestUpdate(b.Nodes[2].NodeID, "starting")}
	require.NoError(t, s.Project(ctx, p))
	gotB, err = s.Read(ctx, "other-owner", b.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, int64(15), gotB.Root.RootSequence)
	require.Equal(t, int64(15), gotB.Nodes[2].ProjectionSequence)
	require.Equal(t, int64(9), gotB.Nodes[1].ProjectionSequence)
	unchangedA, err := s.Read(ctx, "owner", a.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, gotA, unchangedA)
}

func TestBatchCrossRootLateFailureRollback(t *testing.T) {
	for _, failure := range []string{"missing-node", "invalid-state", "foreign-node", "wrong-owner", "wrong-host", "old-incarnation", "stale", "equal", "node-stale", "node-equal"} {
		t.Run(failure, func(t *testing.T) {
			s, ctx, a, b, p := batchStoreTestCrossRoot(t)
			switch failure {
			case "missing-node":
				p.Roots[1].Updates[1].NodeID = "missing"
			case "invalid-state":
				p.Roots[1].Updates[1].State = "unknown"
			case "foreign-node":
				p.Roots[1].Updates[1].NodeID = a.Nodes[1].NodeID
			case "wrong-owner":
				p.Roots[1].Owner = "owner"
			case "wrong-host":
				p.Roots[1].Updates = nil // Isolate the root host fence from node-write guards.
				_, err := s.writer.ExecContext(ctx, "UPDATE batch_nodes SET host_id='other-host' WHERE root_id=?", b.Root.RootID)
				// The same-incarnation FKs disallow changing host piecemeal; update all rows in one statement.
				require.NoError(t, err)
				b, err = s.Read(ctx, "other-owner", b.Root.RootID)
				require.NoError(t, err)
			case "node-stale", "node-equal":
				sequence := int64(9)
				if failure == "node-stale" {
					sequence = 10
				}
				_, err := s.writer.ExecContext(ctx, "UPDATE batch_nodes SET projection_sequence=? WHERE node_id=?", sequence, b.Nodes[1].NodeID)
				require.NoError(t, err)
				b, err = s.Read(ctx, "other-owner", b.Root.RootID)
				require.NoError(t, err)
			case "old-incarnation":
				p.Roots[1].Root = "old-root"
			case "stale":
				p.Sequence = 3
			case "equal":
				p.Sequence = 4
			}
			err := s.Project(ctx, p)
			if failure == "invalid-state" {
				require.ErrorContains(t, err, "CHECK constraint")
			} else {
				require.ErrorIs(t, err, ErrBatchProjectionConflict)
			}
			gotA, err := s.Read(ctx, "owner", a.Root.RootID)
			require.NoError(t, err)
			gotB, err := s.Read(ctx, "other-owner", b.Root.RootID)
			require.NoError(t, err)
			require.Equal(t, a, gotA) // includes outbox, root and node sequences
			require.Equal(t, b, gotB)
		})
	}
}

func TestBatchCrossRootDuplicateAndStaleSequence(t *testing.T) {
	s, ctx, a, b, p := batchStoreTestCrossRoot(t)
	require.NoError(t, s.Project(ctx, p))
	beforeA, err := s.Read(ctx, "owner", a.Root.RootID)
	require.NoError(t, err)
	beforeB, err := s.Read(ctx, "other-owner", b.Root.RootID)
	require.NoError(t, err)
	// Root-only projections isolate the sequence fence from node and notice guards.
	for i := range p.Roots {
		p.Roots[i].Updates, p.Roots[i].Notices = nil, nil
	}
	for _, sequence := range []int64{9, 8} {
		p.Sequence = sequence
		require.ErrorIs(t, s.Project(ctx, p), ErrBatchProjectionConflict)
		gotA, err := s.Read(ctx, "owner", a.Root.RootID)
		require.NoError(t, err)
		gotB, err := s.Read(ctx, "other-owner", b.Root.RootID)
		require.NoError(t, err)
		require.Equal(t, beforeA, gotA)
		require.Equal(t, beforeB, gotB)
	}
}

func TestBatchControlsHostIncarnationAndCascades(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	snap := batchStoreTestReserve(t, s, ctx)
	_, e := s.CreateControl(ctx, "owner", "wrong", snap.Root.RootID, snap.Nodes[1].NodeID)
	require.Error(t, e)
	c, e := s.CreateControl(ctx, "owner", "host", snap.Root.RootID, snap.Nodes[1].NodeID)
	require.NoError(t, e)
	controls, e := s.PendingControls(ctx, "owner", "wrong", snap.Root.RootID)
	require.NoError(t, e)
	require.Empty(t, controls)
	controls, e = s.PendingControls(ctx, "owner", "host", snap.Root.RootID)
	require.NoError(t, e)
	require.Len(t, controls, 1)
	a := db.AnswerBatchControlRequestParams{RequestID: c.RequestID, RootID: c.RootID, NodeID: c.NodeID, OwnerSessionID: c.OwnerSessionID, HostID: "wrong", AnswerCode: "accepted", AnswerSequence: sql.NullInt64{Int64: 2, Valid: true}, AnsweredAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true}}
	require.ErrorIs(t, s.AnswerControl(ctx, a), ErrBatchProjectionConflict)
	a.HostID = "host"
	a.RootID = "old"
	require.ErrorIs(t, s.AnswerControl(ctx, a), ErrBatchProjectionConflict)
	a.RootID = c.RootID
	require.NoError(t, s.AnswerControl(ctx, a))
	require.ErrorIs(t, s.AnswerControl(ctx, a), ErrBatchProjectionConflict)
	require.ErrorIs(t, s.DeleteRoot(ctx, "owner", "wrong", c.RootID), ErrBatchProjectionConflict)
	require.NoError(t, s.DeleteRoot(ctx, "owner", "host", c.RootID))
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM batch_control_requests").Scan(&count))
	require.Zero(t, count)
	snap = batchStoreTestReserve(t, s, ctx)
	_, e = s.CreateControl(ctx, "owner", "host", snap.Root.RootID, snap.Nodes[1].NodeID)
	require.NoError(t, e)
	_, e = conn.ExecContext(ctx, "DELETE FROM sessions WHERE id='owner'")
	require.NoError(t, e)
	for _, table := range []string{"batch_nodes", "batch_control_requests"} {
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count))
		require.Zero(t, count)
	}
}
