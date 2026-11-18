// Test-support helpers for the external-sessionhosts_test lock tests,
// copied byte-for-byte from package session (host_store_test.go) when the
// host store moved to this state package. Only package-level qualifiers
// changed (sessionhosts.NewHostStore -> sessionhosts.NewHostStore via the
// external import alias).
package sessionhosts_test

import (
	"context"
	"database/sql"
	"testing"

	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
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
func newHostTestStore(t *testing.T) (*sessionhosts.HostStore, *sql.DB, *db.Queries, string, context.Context) {
	t.Helper()
	conn, q, dataDir, ctx := newHostTestDB(t)
	store := sessionhosts.NewHostStore(conn, dataDir, 999, "test")
	// Release the host's OS lock file (if still held) before t.TempDir()'s
	// cleanup -- Windows refuses to delete a file this process holds open.
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store, conn, q, dataDir, ctx
}
