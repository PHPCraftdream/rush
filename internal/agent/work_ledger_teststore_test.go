// Shared test helper for the phase-4 step-2 migration: every isolated
// ledger test that starts a non-sync async job now needs a real, durable
// job store behind it (doc plan sec.5 step 2 -- Start's fail-closed Claim
// call, DUR-8), not a nil-store mode. One helper, one real temp-SQLite DB
// per test, matches the existing newWorkLedger(nil) isolation model: each
// caller gets its own fresh data dir, never shared across test functions.
package agent

import (
	"context"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// newTestAsyncJobStore builds an AsyncJobStore backed by a fresh, migrated
// SQLite database in a per-test temp dir. Host registration stays lazy (the
// store's own contract) -- this helper does not force it.
//
// Foreign keys are turned OFF on this connection deliberately: these are
// isolated ledger unit tests (no session.Service wired, exactly like their
// pre-existing nil-store fixtures), and they use ad hoc owner ids ("owner",
// "session-1", ...) that were never real sessions rows even before this
// step -- async_jobs.owner_session_id's FK exists for production integrity
// (cascade delete), not for this package's unit tests to satisfy by hand.
// The session package's own AsyncJobStore tests (internal/session) DO seed
// real sessions rows and keep foreign_keys ON, matching that package's
// existing convention.
func newTestAsyncJobStore(t *testing.T) *session.AsyncJobStore {
	t.Helper()
	dataDir := t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	store := session.NewAsyncJobStore(conn, dataDir, os.Getpid(), "test")
	// Release the host's OS lock file before t.TempDir()'s own cleanup tries
	// to remove the directory -- otherwise Windows refuses to delete a file
	// this same process still holds open.
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}
