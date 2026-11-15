package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupStoreV1DB migrates a fresh temp SQLite DB to the embedded set and
// inserts one session row (FK target for session_notices.owner and
// batch_nodes.owner_session_id).
func setupStoreV1DB(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	conn, err := openDB(t.TempDir() + "/store_v1.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, Migrate(ctx, conn))
	_, err = conn.ExecContext(ctx,
		`INSERT INTO sessions (id, title, updated_at, created_at) VALUES (?, ?, ?, ?)`,
		"sess-1", "sess-1", int64(1700000000), int64(1700000000),
	)
	require.NoError(t, err)
	return ctx, conn
}

func requireTableExists(t *testing.T, ctx context.Context, conn *sql.DB, name string, want bool) {
	t.Helper()
	var count int
	err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&count)
	require.NoError(t, err)
	if want {
		assert.Equal(t, 1, count, "expected table %s to exist", name)
	} else {
		assert.Equal(t, 0, count, "expected table %s to be dropped", name)
	}
}

// TestSessionNotices_OwnerCascadeDelete proves the ON DELETE CASCADE FK on
// session_notices.owner (doc sec.3.2/3.8): deleting the owning session
// removes its notice rows instead of leaving them orphaned or blocking the
// delete.
func TestSessionNotices_OwnerCascadeDelete(t *testing.T) {
	ctx, conn := setupStoreV1DB(t)
	q := New(conn)

	_, err := q.InsertSessionNotice(ctx, InsertSessionNoticeParams{
		Owner: "sess-1", Kind: "supervision", Text: "t", Wake: 1,
		CreatedAt: 1700000000, UpdatedAt: 1700000000,
	})
	require.NoError(t, err)

	_, err = conn.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, "sess-1")
	require.NoError(t, err)

	notices, err := q.ListSessionNoticesForOwner(ctx, "sess-1")
	require.NoError(t, err)
	assert.Empty(t, notices, "session_notices rows must be cascade-deleted with their owning session")
}

// TestDeleteAsyncHostIfNoNodes is the store-v1 translation of the correlated-
// subquery host-guard regression test: the guard's subquery is correlated
// against async_hosts.id instead of re-binding a second named parameter (the
// workaround for a real sqlc v1.30.0 code-generation defect). This test
// proves the ACTUAL runtime behavior against batch_nodes: a host owning a
// live node row survives, one with zero live nodes is deleted.
func TestDeleteAsyncHostIfNoNodes(t *testing.T) {
	ctx, conn := setupStoreV1DB(t)
	q := New(conn)

	_, err := q.RegisterAsyncHostRow(ctx, RegisterAsyncHostRowParams{ID: "host-busy", Pid: 1, Label: "app", StartedAt: 1700000000})
	require.NoError(t, err)
	_, err = q.RegisterAsyncHostRow(ctx, RegisterAsyncHostRowParams{ID: "host-idle", Pid: 2, Label: "app", StartedAt: 1700000000})
	require.NoError(t, err)

	// One LIVE batch_nodes root row owned by host-busy (the schema's CHECKs
	// force this exact root shape).
	_, err = conn.ExecContext(ctx,
		`INSERT INTO batch_nodes (node_id, root_id, owner_session_id, host_id, parent_id, ordinal, kind,
			tool_call_id, input_hash, mode, state, created_at, updated_at)
			VALUES ('root-1', 'root-1', 'sess-1', 'host-busy', NULL, 0, 'batch', 'call-r', 'hash-r',
			'parallel', 'pending', 1700000000, 1700000000)`)
	require.NoError(t, err)

	rows, err := q.DeleteAsyncHostIfNoNodes(ctx, "host-busy")
	require.NoError(t, err)
	assert.EqualValues(t, 0, rows, "a host owning a live batch_nodes row must NOT be deleted")

	rows, err = q.DeleteAsyncHostIfNoNodes(ctx, "host-idle")
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows, "a host with zero live batch_nodes rows must be deleted")

	remaining, err := q.ListAsyncHostRows(ctx)
	require.NoError(t, err)
	ids := make([]string, len(remaining))
	for i, h := range remaining {
		ids[i] = h.ID
	}
	assert.Contains(t, ids, "host-busy")
	assert.NotContains(t, ids, "host-idle")
}
