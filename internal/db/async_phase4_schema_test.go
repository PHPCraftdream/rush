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

	// Step down to the version immediately before the phase-4 core migration
	// (20260928000002) by its own timestamp, not just "one migration" -- a
	// LATER migration (20260929000001, the A9 session_notices.owner index)
	// now sits on top of it, so "down exactly one" would only undo the
	// index, not the phase-4 tables. DownTo is exact regardless of how many
	// migrations have landed after phase-4 core since.
	_, err = provider.DownTo(ctx, 20260928000001)
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
	// Realistic ack-gate ordering: "started" commits (MarkAsyncJobAnnounced)
	// before the job can possibly reach terminal.
	_, err = q.MarkAsyncJobAnnounced(ctx, MarkAsyncJobAnnouncedParams{
		UpdatedAt: 1700000000, OwnerSessionID: "sess-1", ToolCallID: "call-1",
	})
	require.NoError(t, err)
	_, err = q.TransitionAsyncJobTerminalPreserveVoid(ctx, TransitionAsyncJobTerminalPreserveVoidParams{
		State: "completed", NoticeKind: "", ResultSummary: sql.NullString{String: "ok", Valid: true},
		ResultIsError: sql.NullInt64{Int64: 0, Valid: true}, Wake: 1, UpdatedAt: 1700000001,
		Delivery:       "pending",
		OwnerSessionID: "sess-1", ToolCallID: "call-1",
	})
	require.NoError(t, err)

	debt, err = q.AsyncReactionDebtExists(ctx, "sess-1")
	require.NoError(t, err)
	assert.True(t, debt.Bool, "announced=1, wake=1, delivery=pending, reacted=0 must report debt")

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

// TestAsyncReactionDebtExists_RequiresAnnounced is a regression test for the
// Ф4-1 review's P1 finding: a job that reaches terminal before its own
// "started" tool-result commits is wake=1/delivery=pending/reacted=0 but
// announced=0. Doc sec.3.4 is explicit the debt check is scoped to "task
// rows with announced=1" -- an unannounced row can never produce a notice
// (DUR-7) and the drain pull already skips it (ListPendingAsyncJobNoticesForOwner/
// PullPendingAsyncJobNotice both require announced=1), so without this
// guard the debt check would falsely report debt, driving a wasted
// provider turn with an empty prompt and nothing to react to.
func TestAsyncReactionDebtExists_RequiresAnnounced(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)
	_, err := q.RegisterAsyncHost(ctx, RegisterAsyncHostParams{ID: "host-1", Pid: 1, StartedAt: 1700000000})
	require.NoError(t, err)

	_, err = q.ClaimAsyncJob(ctx, ClaimAsyncJobParams{
		OwnerSessionID: "sess-1", ToolCallID: "call-1", Kind: "command",
		InputHash: "h", HostID: "host-1", CreatedAt: 1700000000, UpdatedAt: 1700000000,
	})
	require.NoError(t, err)

	// Terminal transition races ahead of the ack gate: the row is
	// wake=1/delivery=pending/reacted=0 but still announced=0.
	_, err = q.TransitionAsyncJobTerminalPreserveVoid(ctx, TransitionAsyncJobTerminalPreserveVoidParams{
		State: "completed", NoticeKind: "", ResultSummary: sql.NullString{String: "ok", Valid: true},
		ResultIsError: sql.NullInt64{Int64: 0, Valid: true}, Wake: 1, UpdatedAt: 1700000001,
		Delivery:       "pending",
		OwnerSessionID: "sess-1", ToolCallID: "call-1",
	})
	require.NoError(t, err)

	debt, err := q.AsyncReactionDebtExists(ctx, "sess-1")
	require.NoError(t, err)
	assert.False(t, debt.Bool, "an unannounced terminal row must NOT count as debt")

	_, err = q.MarkAsyncJobAnnounced(ctx, MarkAsyncJobAnnouncedParams{
		UpdatedAt: 1700000002, OwnerSessionID: "sess-1", ToolCallID: "call-1",
	})
	require.NoError(t, err)

	debt, err = q.AsyncReactionDebtExists(ctx, "sess-1")
	require.NoError(t, err)
	assert.True(t, debt.Bool, "once announced=1, the same row must count as debt")
}

// TestDeleteAsyncHostIfNoJobs_CorrelatedSubquery is a regression test for a
// real sqlc v1.30.0 code-generation defect found while writing this query:
// passing the same id value to a bare `?`, a repeated @id, or two
// distinctly-named args all produced a query that either bound the wrong
// number of values or left one placeholder as invalid literal text (see the
// comment on DeleteAsyncHostIfNoJobs in sql/async_jobs.sql). The fix
// correlates the subquery against async_hosts.id instead of re-binding a
// second parameter. This test proves the ACTUAL runtime behavior: a host
// with a referencing row survives, one with none is deleted.
func TestDeleteAsyncHostIfNoJobs_CorrelatedSubquery(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)

	_, err := q.RegisterAsyncHost(ctx, RegisterAsyncHostParams{ID: "host-busy", Pid: 1, StartedAt: 1700000000})
	require.NoError(t, err)
	_, err = q.RegisterAsyncHost(ctx, RegisterAsyncHostParams{ID: "host-idle", Pid: 2, StartedAt: 1700000000})
	require.NoError(t, err)

	require.NoError(t, insertRunningAsyncJob(t, ctx, conn, q, "call-1", "", "host-busy"))

	rows, err := q.DeleteAsyncHostIfNoJobs(ctx, "host-busy")
	require.NoError(t, err)
	assert.EqualValues(t, 0, rows, "a host with a referencing async_jobs row must NOT be deleted")

	rows, err = q.DeleteAsyncHostIfNoJobs(ctx, "host-idle")
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows, "a host with zero referencing rows must be deleted")

	remaining, err := q.ListAsyncHosts(ctx)
	require.NoError(t, err)
	ids := make([]string, len(remaining))
	for i, h := range remaining {
		ids[i] = h.ID
	}
	assert.Contains(t, ids, "host-busy")
	assert.NotContains(t, ids, "host-idle")
}
