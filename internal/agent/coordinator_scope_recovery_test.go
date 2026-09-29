// Own-scope recovery (doc sec.3.5/3.7, step 5): ScopeOpen recovers its
// owner's dead-host rows to 'interrupted' before answering, so a row whose
// host just died is treated as recoverable, never as open scope forever.
package agent

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// newTestAsyncJobStoreWithDataDir is newTestAsyncJobStore plus the raw
// *sql.DB and data dir, needed here to fabricate a dead host's lock file and
// seed a running row directly (session.AsyncJobStore.Claim always registers
// THIS process's own host id, never an arbitrary one).
func newTestAsyncJobStoreWithDataDir(t *testing.T) (*session.AsyncJobStore, string, *sql.DB) {
	t.Helper()
	dataDir := t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	store := session.NewAsyncJobStore(conn, dataDir, os.Getpid(), "test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store, dataDir, conn
}

// TestScopeOpen_RecoversDeadHostRowBeforeAnswering pins the CLI-loop wiring
// (app_run_async.go's waitForNextCLITurn calls ScopeOpen between turns): a
// row on a host that has since died must be recovered to 'interrupted' as
// part of answering the scope question, not merely skipped by HostNotDead
// and left 'running' forever.
func TestScopeOpen_RecoversDeadHostRowBeforeAnswering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
	q := db.New(conn)

	seed, err := session.TryAcquireFileLock(session.HostLockPath(dataDir, "dead-host-x"))
	require.NoError(t, err)
	require.NoError(t, seed.Release())

	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
		OwnerSessionID: "sess-1", ToolCallID: "call-1", Kind: "command", ToolName: "bash",
		InputHash: "h1", HostID: "dead-host-x", CreatedAt: 1700000000, UpdatedAt: 1700000000,
	})
	require.NoError(t, err)
	_, err = q.MarkAsyncJobAnnounced(ctx, db.MarkAsyncJobAnnouncedParams{
		UpdatedAt: 1700000001, OwnerSessionID: "sess-1", ToolCallID: "call-1",
	})
	require.NoError(t, err)

	c := &coordinator{}
	c.asyncJobs = newWorkLedger(nil)
	c.asyncJobs.store = store

	open, err := c.ScopeOpen(ctx, "sess-1")
	require.NoError(t, err)
	require.False(t, open, "an interrupted row (wake=0) with nothing else outstanding must close the scope")

	row, err := store.Get(ctx, "sess-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "interrupted", row.State, "ScopeOpen must have recovered the dead host's row, not merely skipped it")
	require.Equal(t, "interrupted", row.NoticeKind)
	require.EqualValues(t, 0, row.Wake)
}

// TestScopeOpen_LiveHostRunningRowKeepsScopeOpen is the negative twin: a
// running row on a LIVE host must still report scope open, unrecovered.
func TestScopeOpen_LiveHostRunningRowKeepsScopeOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, dataDir, conn := newTestAsyncJobStoreWithDataDir(t)
	q := db.New(conn)

	holder, err := session.TryAcquireFileLock(session.HostLockPath(dataDir, "live-host-x"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Release() })

	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
		OwnerSessionID: "sess-1", ToolCallID: "call-1", Kind: "command", ToolName: "bash",
		InputHash: "h1", HostID: "live-host-x", CreatedAt: 1700000000, UpdatedAt: 1700000000,
	})
	require.NoError(t, err)

	c := &coordinator{}
	c.asyncJobs = newWorkLedger(nil)
	c.asyncJobs.store = store

	open, err := c.ScopeOpen(ctx, "sess-1")
	require.NoError(t, err)
	require.True(t, open, "a running row on a live host must keep the scope open")

	row, err := store.Get(ctx, "sess-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "running", row.State, "a live host's row must never be recovered")
}
