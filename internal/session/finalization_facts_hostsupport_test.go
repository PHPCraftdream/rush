// Test-support helpers copied byte-for-byte from the sessionhosts state
// package (session_driver_test.go / host_testkit_test.go) when the host
// store moved there: package session's finalization-facts hold tests need
// the same fixture on this side, and the lower package's test helpers are
// not visible across the boundary. Only package-level qualifiers changed
// (NewHostStore -> sessionhosts.NewHostStore, hostNodeStore -> the
// exported AsyncHostStoreFromQueries seam).
package session

import (
	"context"
	"database/sql"
	"testing"

	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/db/storepath"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// nodeStore routes a *db.Queries through the retained adapter onto
// sessionhosts.AsyncHostStore: *db.Queries itself no longer satisfies the
// interface (its legacy async_jobs host methods were deleted), so every
// sessionhosts.RegisterHost/Close call in these tests must go through the
// adapter.
func nodeStore(q *db.Queries) sessionhosts.AsyncHostStore {
	return sessionhosts.AsyncHostStoreFromQueries(q)
}

// nStores opens n independent connections to ONE database file/data dir, each
// wrapped in its own HostStore (its own host identity once it claims),
// with every named session pre-seeded.
func nStores(t *testing.T, n int, sessionIDs ...string) (stores []*sessionhosts.HostStore, q *db.Queries, ctx context.Context, dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	ctx = context.Background()
	setup, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	for _, id := range sessionIDs {
		require.NoError(t, seedSession(ctx, db.New(setup), id))
	}
	require.NoError(t, db.Release(dataDir))

	path := storepath.DataDirPath(dataDir, storepath.ActiveFilename)
	for i := 0; i < n; i++ {
		conn, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
		require.NoError(t, err)
		conn.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = conn.Close() })
		st := sessionhosts.NewHostStore(conn, dataDir, 1000+i, "store")
		t.Cleanup(func() { _ = st.Close(context.Background()) })
		stores = append(stores, st)
	}
	return stores, stores[0].QueriesForTest(), ctx, dataDir
}

// seedSession already exists in this package (session_testkit_test.go); the
// copied nStores calls it as it did in the sessionhosts package.

// seedSession and countMessages: copied byte-for-byte from sessionhosts (host_lock_test.go)
// and sessionwake tests so the batch tests need no cross-package helpers.
func seedSession(ctx context.Context, q *db.Queries, id string) error {
	_, err := q.CreateSession(ctx, db.CreateSessionParams{ID: id, Title: id})
	return err
}

// countMessages is the read-side helper the wake-schedule tests (now in
// package sessionwake_test) also carry; copied byte-for-byte so the batch
// pull tests keep their message-count assertions without crossing the
// package boundary.
func countMessages(t *testing.T, q *db.Queries, ctx context.Context, sessionID string) int64 {
	t.Helper()
	n, err := q.CountMessagesBySession(ctx, sessionID)
	require.NoError(t, err)
	return n
}
