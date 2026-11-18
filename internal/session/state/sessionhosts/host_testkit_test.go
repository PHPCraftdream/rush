// Test-support helpers copied byte-for-byte from package session
// (host_store_test.go) when the host store moved to this state package:
// newHostTestStore is unexported there, and host_driver_test.go needs the
// same fixture on this side. Only package-level qualifiers changed
// (sessionhosts.NewHostStore -> NewHostStore).
package sessionhosts

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/stretchr/testify/require"
)

// newHostTestDB opens a real, migrated SQLite DB via db.Connect in a fresh
// temp dir, then drops async_jobs when it still exists. Connect itself only
// runs migrations (goose Up) and opens the pool -- nothing after Connect in
// these tests reads async_jobs, so the drop is safe immediately after it
// returns and turns any hidden dependency into a loud "no such table" error.
func newHostTestDB(t *testing.T) (*sql.DB, *db.Queries, string, context.Context) {
	t.Helper()
	dataDir := t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	_, err = conn.ExecContext(ctx, "DROP TABLE IF EXISTS async_jobs")
	require.NoError(t, err, "the async_jobs drop must always succeed")
	return conn, db.New(conn), dataDir, ctx
}

// newHostTestStore builds a HostStore over newHostTestDB's connection.
func newHostTestStore(t *testing.T) (*HostStore, *sql.DB, *db.Queries, string, context.Context) {
	t.Helper()
	conn, q, dataDir, ctx := newHostTestDB(t)
	store := NewHostStore(conn, dataDir, 999, "test")
	// Release the host's OS lock file (if still held) before t.TempDir()'s
	// cleanup -- Windows refuses to delete a file this process holds open.
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store, conn, q, dataDir, ctx
}

func seedSession(ctx context.Context, q *db.Queries, id string) error {
	_, err := q.CreateSession(ctx, db.CreateSessionParams{ID: id, Title: id})
	return err
}

// setupHostLockDB opens a real, migrated SQLite DB for host-lock tests that
// touch async_hosts/async_jobs (doc sec.3.6 requires real-DB coverage, not
// a fake). Copied byte-for-byte from host_lock_test.go when that file moved
// to the external sessionhosts_test package.
func setupHostLockDB(t *testing.T) (context.Context, *db.Queries) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	return ctx, db.New(conn)
}

// nodeStore routes a *db.Queries through the retained hostNodeStore adapter
// (host_store.go) onto AsyncHostStore: *db.Queries itself no longer satisfies
// the interface (its legacy async_jobs host methods were deleted), so every
// sessionhosts.RegisterHost/Close call in these tests must go through the adapter.
func nodeStore(q *db.Queries) AsyncHostStore { return hostNodeStore{q: q} }
