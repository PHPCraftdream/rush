package session

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/store/db"
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

// batchReaderTestDBs opens a writer pool and a WAL read pool on the same
// fresh database and releases BOTH via t.Cleanup (db.ReleaseConn).
func batchReaderTestDBs(t *testing.T) (ctx context.Context, w, r *sql.DB) {
	t.Helper()
	ctx = t.Context()
	dataDir := t.TempDir()
	var err error
	w, err = db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(w)) })
	r, err = db.ConnectRead(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(r)) })
	// batch_nodes.owner_session_id references sessions(id); seed the owner
	// through the writer like the other BatchStore fixtures.
	require.NoError(t, seedSession(ctx, db.New(w), "owner"))
	return ctx, w, r
}
