package db

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupPhase4DB migrates a fresh temp SQLite DB and inserts one session row
// (FK target for async_jobs.owner_session_id / session_notices.owner).
func setupPhase4DB(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	conn, err := openDB(t.TempDir() + "/phase4.db")
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

func insertRunningAsyncJob(t *testing.T, ctx context.Context, conn *sql.DB, q *Queries, toolCallID, childSessionID, hostID string) error {
	t.Helper()
	var child sql.NullString
	if childSessionID != "" {
		child = sql.NullString{String: childSessionID, Valid: true}
	}
	_, err := q.ClaimAsyncJob(ctx, ClaimAsyncJobParams{
		OwnerSessionID: "sess-1",
		ToolCallID:     toolCallID,
		Kind:           "agent",
		InputHash:      "h",
		ChildSessionID: child,
		OriginCli:      0,
		HostID:         hostID,
		CreatedAt:      1700000000,
		UpdatedAt:      1700000000,
	})
	return err
}

// TestAsyncPhase4Migration_UpCreatesTables is the up-migration smoke test
// (doc sec.5 step 0): all three phase-4 tables and their columns exist and
// are usable via the generated sqlc queries.
func TestAsyncPhase4Migration_UpCreatesTables(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)

	host, err := q.RegisterAsyncHost(ctx, RegisterAsyncHostParams{
		ID: "host-1", Pid: 123, Label: "cli", StartedAt: 1700000000,
	})
	require.NoError(t, err)
	assert.Equal(t, "host-1", host.ID)

	job, err := q.ClaimAsyncJob(ctx, ClaimAsyncJobParams{
		OwnerSessionID: "sess-1",
		ToolCallID:     "call-1",
		Kind:           "command",
		InputHash:      "hash1",
		HostID:         "host-1",
		CreatedAt:      1700000000,
		UpdatedAt:      1700000000,
	})
	require.NoError(t, err)
	assert.Equal(t, "running", job.State)
	assert.Equal(t, "none", job.Delivery)
	assert.EqualValues(t, 0, job.Announced)
	assert.EqualValues(t, 0, job.Reacted)

	notice, err := q.InsertSessionNotice(ctx, InsertSessionNoticeParams{
		Owner:     "sess-1",
		Kind:      "supervision",
		Text:      "hello",
		Wake:      1,
		CreatedAt: 1700000000,
		UpdatedAt: 1700000000,
	})
	require.NoError(t, err)
	assert.Equal(t, "pending", notice.Delivery)
}

// TestAsyncPhase4Migration_DownDropsTables proves the down migration is the
// exact inverse of up: after DownTo(previous version), none of the phase-4
// tables exist, and after Up-ing again they are back and usable. Uses a
// direct goose.Provider (not the package's Migrate helper, which only goes
// Up) to drive one specific migration down and back up.
func TestAsyncPhase4Migration_DownDropsTables(t *testing.T) {
	ctx := context.Background()
	conn, err := openDB(t.TempDir() + "/phase4_updown.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	migrationFS, err := fs.Sub(FS, "migrations")
	require.NoError(t, err)
	provider, err := goose.NewProvider(goose.DialectSQLite3, conn, migrationFS, goose.WithLogger(goose.NopLogger()))
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)
	requireTableExists(t, ctx, conn, "async_jobs", true)
	requireTableExists(t, ctx, conn, "async_hosts", true)
	requireTableExists(t, ctx, conn, "session_notices", true)

	// Step down exactly one migration -- this phase-4 migration is the most
	// recently applied one (highest timestamp in internal/db/migrations).
	_, err = provider.Down(ctx)
	require.NoError(t, err)
	requireTableExists(t, ctx, conn, "async_jobs", false)
	requireTableExists(t, ctx, conn, "async_hosts", false)
	requireTableExists(t, ctx, conn, "session_notices", false)

	// Back up: proves Down did not leave stray objects (e.g. an index)
	// that would make a repeat Up fail with "already exists".
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	requireTableExists(t, ctx, conn, "async_jobs", true)
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

// TestAsyncJobs_ChildSessionUniquePartialIndex is a revert-check-backed
// regression test for ASYNC-01 (doc sec.3.8): at most one RUNNING async_jobs
// row may claim a given child_session_id. Neutering the migration's WHERE
// clause (making the index non-partial, or dropping it) makes this fail --
// verified by hand: temporarily changing the CREATE UNIQUE INDEX condition
// to `WHERE 0` (never applies) reproduces exactly the failure this test
// guards against, then the migration was restored byte-for-byte (git diff
// clean) before this test was left in place.
func TestAsyncJobs_ChildSessionUniquePartialIndex(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)
	_, err := q.RegisterAsyncHost(ctx, RegisterAsyncHostParams{ID: "host-1", Pid: 1, StartedAt: 1700000000})
	require.NoError(t, err)

	require.NoError(t, insertRunningAsyncJob(t, ctx, conn, q, "call-a", "child-1", "host-1"))

	err = insertRunningAsyncJob(t, ctx, conn, q, "call-b", "child-1", "host-1")
	require.Error(t, err, "a second RUNNING row must not be able to claim the same child_session_id (ASYNC-01)")

	// A second row may still be inserted once the first is no longer
	// 'running': the partial index does not constrain terminal rows.
	res, err := conn.ExecContext(ctx,
		`UPDATE async_jobs SET state = 'completed', delivery = 'pending', updated_at = ? WHERE tool_call_id = 'call-a'`,
		int64(1700000001),
	)
	require.NoError(t, err)
	rows, _ := res.RowsAffected()
	require.EqualValues(t, 1, rows)

	require.NoError(t, insertRunningAsyncJob(t, ctx, conn, q, "call-c", "child-1", "host-1"),
		"a new RUNNING claim on the same child must succeed once the prior claim is terminal")
}

// TestSessionNotices_OwnerCascadeDelete proves the ON DELETE CASCADE FK on
// session_notices.owner (doc sec.3.2/3.8): deleting the owning session
// removes its notice rows instead of leaving them orphaned or blocking the
// delete.
func TestSessionNotices_OwnerCascadeDelete(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
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

// TestAsyncReactionDebtExists exercises the single doc-sec.3.4 debt check
// across both tables: no debt initially, debt appears from an async_jobs
// row, clearing it (reacted=1) removes it, then a session_notices row alone
// still reports debt.
func TestAsyncReactionDebtExists(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)
	_, err := q.RegisterAsyncHost(ctx, RegisterAsyncHostParams{ID: "host-1", Pid: 1, StartedAt: 1700000000})
	require.NoError(t, err)

	debt, err := q.AsyncReactionDebtExists(ctx, "sess-1")
	require.NoError(t, err)
	assert.False(t, debt.Bool, "no rows yet -- no debt")

	_, err = q.ClaimAsyncJob(ctx, ClaimAsyncJobParams{
		OwnerSessionID: "sess-1", ToolCallID: "call-1", Kind: "command",
		InputHash: "h", HostID: "host-1", CreatedAt: 1700000000, UpdatedAt: 1700000000,
	})
	require.NoError(t, err)
	_, err = q.TransitionAsyncJobTerminal(ctx, TransitionAsyncJobTerminalParams{
		State: "completed", NoticeKind: "", ResultSummary: sql.NullString{String: "ok", Valid: true},
		ResultIsError: sql.NullInt64{Int64: 0, Valid: true}, Wake: 1, UpdatedAt: 1700000001,
		OwnerSessionID: "sess-1", ToolCallID: "call-1",
	})
	require.NoError(t, err)

	debt, err = q.AsyncReactionDebtExists(ctx, "sess-1")
	require.NoError(t, err)
	assert.True(t, debt.Bool, "wake=1, delivery=pending, reacted=0 must report debt")

	rows, err := q.MarkAsyncJobsReactedForOwner(ctx, MarkAsyncJobsReactedForOwnerParams{
		UpdatedAt: 1700000002, OwnerSessionID: "sess-1",
	})
	require.NoError(t, err)
	assert.EqualValues(t, 0, rows, "delivery is still 'pending' (never pulled to 'done'), so nothing qualifies yet")

	_, err = conn.ExecContext(ctx, `UPDATE async_jobs SET delivery = 'done' WHERE tool_call_id = 'call-1'`)
	require.NoError(t, err)
	rows, err = q.MarkAsyncJobsReactedForOwner(ctx, MarkAsyncJobsReactedForOwnerParams{
		UpdatedAt: 1700000003, OwnerSessionID: "sess-1",
	})
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows)

	debt, err = q.AsyncReactionDebtExists(ctx, "sess-1")
	require.NoError(t, err)
	assert.False(t, debt.Bool, "reacted=1 must clear the debt")

	// A session_notices row alone (no async_jobs row) must also report debt
	// -- the check spans both tables (doc sec.3.4).
	_, err = q.InsertSessionNotice(ctx, InsertSessionNoticeParams{
		Owner: "sess-1", Kind: "supervision", Text: "t", Wake: 1,
		CreatedAt: 1700000004, UpdatedAt: 1700000004,
	})
	require.NoError(t, err)
	debt, err = q.AsyncReactionDebtExists(ctx, "sess-1")
	require.NoError(t, err)
	assert.True(t, debt.Bool)
}
