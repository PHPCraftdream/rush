package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeDoneAsyncJob claims and terminal-transitions a job to a
// wake=1/delivery=done/reacted=0/announced=1 state -- the shape a settle-by-
// failure pass's captured id set is built from (doc sec.3.4: "only for the
// rows that were done at the moment [the failed turn] started").
func makeDoneAsyncJob(t *testing.T, ctx context.Context, q *Queries, owner, toolCallID, hostID string) {
	t.Helper()
	_, err := q.ClaimAsyncJob(ctx, ClaimAsyncJobParams{
		OwnerSessionID: owner, ToolCallID: toolCallID, Kind: "command",
		InputHash: "h", HostID: hostID, CreatedAt: 1700000000, UpdatedAt: 1700000000,
	})
	require.NoError(t, err)
	_, err = q.MarkAsyncJobAnnounced(ctx, MarkAsyncJobAnnouncedParams{
		UpdatedAt: 1700000000, OwnerSessionID: owner, ToolCallID: toolCallID,
	})
	require.NoError(t, err)
	_, err = q.TransitionAsyncJobTerminal(ctx, TransitionAsyncJobTerminalParams{
		State: "completed", NoticeKind: "", ResultSummary: sql.NullString{String: "ok", Valid: true},
		ResultIsError: sql.NullInt64{Int64: 0, Valid: true}, Wake: 1, UpdatedAt: 1700000001,
		OwnerSessionID: owner, ToolCallID: toolCallID,
	})
	require.NoError(t, err)
	// Terminal transition lands delivery='pending'; the drain pulls it to
	// 'done' before a turn ever sees it -- settle-by-failure only touches
	// rows that were 'done', so move it there directly for the test.
	_, err = conn2(t, q).ExecContext(ctx, `UPDATE async_jobs SET delivery = 'done' WHERE owner_session_id = ? AND tool_call_id = ?`, owner, toolCallID)
	require.NoError(t, err)
}

// conn2 recovers the *sql.DB a *Queries was built from, for the one raw
// UPDATE makeDoneAsyncJob needs (there is no PullPendingAsyncJobNotice-free
// way to move a row straight to delivery='done' without a message insert).
func conn2(t *testing.T, q *Queries) *sql.DB {
	t.Helper()
	c, ok := q.db.(*sql.DB)
	require.True(t, ok, "test helper expects a *Queries built directly over a *sql.DB")
	return c
}

func TestIncrementAsyncJobWakeAttempts_ScopedToIDSet(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)
	_, err := q.RegisterAsyncHost(ctx, RegisterAsyncHostParams{ID: "host-1", Pid: 1, StartedAt: 1700000000})
	require.NoError(t, err)

	makeDoneAsyncJob(t, ctx, q, "sess-1", "call-1", "host-1")
	makeDoneAsyncJob(t, ctx, q, "sess-1", "call-2", "host-1")

	rows, err := q.IncrementAsyncJobWakeAttempts(ctx, IncrementAsyncJobWakeAttemptsParams{
		UpdatedAt: 1700000002, OwnerSessionID: "sess-1", ToolCallIds: []string{"call-1"},
	})
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows)

	j1, err := q.GetAsyncJob(ctx, GetAsyncJobParams{OwnerSessionID: "sess-1", ToolCallID: "call-1"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, j1.WakeAttempts)

	j2, err := q.GetAsyncJob(ctx, GetAsyncJobParams{OwnerSessionID: "sess-1", ToolCallID: "call-2"})
	require.NoError(t, err)
	assert.EqualValues(t, 0, j2.WakeAttempts, "a row outside the captured id set must not be incremented")
}

// TestSettleAsyncJobsReactedFailed_ScopedToIDSet is the coordinator-
// requested regression test: settle-by-failure must close debt ONLY for
// the id set captured at the start of the failed turn, never for a row
// that became 'done' afterward -- doc sec.3.4: "только для строк, которые
// были done на момент его начала" (only for rows that were done at the
// moment it started).
//
// REVERT CHECK: change SettleAsyncJobsReactedFailed's WHERE clause in
// sql/async_jobs.sql to drop `tool_call_id IN (sqlc.slice('tool_call_ids'))`
// (settling every wake=1/reacted=0 row of the owner instead), regenerate
// with sqlc, and this test's second assertion fails (call-2 is wrongly
// settled too). Verified in this session; reverted back before commit.
func TestSettleAsyncJobsReactedFailed_ScopedToIDSet(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)
	_, err := q.RegisterAsyncHost(ctx, RegisterAsyncHostParams{ID: "host-1", Pid: 1, StartedAt: 1700000000})
	require.NoError(t, err)

	// call-1 is done and captured in the failed turn's id set.
	makeDoneAsyncJob(t, ctx, q, "sess-1", "call-1", "host-1")

	// call-2 becomes done ONLY AFTER the id set was captured -- it must not
	// be swept up by the settle even though it now matches the same
	// wake=1/reacted=0 shape.
	makeDoneAsyncJob(t, ctx, q, "sess-1", "call-2", "host-1")

	rows, err := q.SettleAsyncJobsReactedFailed(ctx, SettleAsyncJobsReactedFailedParams{
		UpdatedAt: 1700000003, OwnerSessionID: "sess-1", ToolCallIds: []string{"call-1"},
	})
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows)

	j1, err := q.GetAsyncJob(ctx, GetAsyncJobParams{OwnerSessionID: "sess-1", ToolCallID: "call-1"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, j1.Reacted)
	assert.EqualValues(t, 1, j1.ReactedFailed)

	j2, err := q.GetAsyncJob(ctx, GetAsyncJobParams{OwnerSessionID: "sess-1", ToolCallID: "call-2"})
	require.NoError(t, err)
	assert.EqualValues(t, 0, j2.Reacted, "a row outside the captured id set must not be settled")
	assert.EqualValues(t, 0, j2.ReactedFailed, "a row outside the captured id set must not be marked reacted_failed")

	debt, err := q.AsyncReactionDebtExists(ctx, "sess-1")
	require.NoError(t, err)
	assert.True(t, debt.Bool, "call-2's own debt must survive the settle untouched")
}

func TestListReactedFailedAsyncJobsForOwner(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)
	_, err := q.RegisterAsyncHost(ctx, RegisterAsyncHostParams{ID: "host-1", Pid: 1, StartedAt: 1700000000})
	require.NoError(t, err)

	makeDoneAsyncJob(t, ctx, q, "sess-1", "call-1", "host-1")
	makeDoneAsyncJob(t, ctx, q, "sess-1", "call-2", "host-1")

	_, err = q.SettleAsyncJobsReactedFailed(ctx, SettleAsyncJobsReactedFailedParams{
		UpdatedAt: 1700000003, OwnerSessionID: "sess-1", ToolCallIds: []string{"call-1"},
	})
	require.NoError(t, err)

	failed, err := q.ListReactedFailedAsyncJobsForOwner(ctx, "sess-1")
	require.NoError(t, err)
	require.Len(t, failed, 1, "only the settled row must appear -- a real reaction never sets reacted_failed")
	assert.Equal(t, "call-1", failed[0].ToolCallID)
}

// --- session_notices half ---

func makeDoneSessionNotice(t *testing.T, ctx context.Context, q *Queries, owner, text string) int64 {
	t.Helper()
	n, err := q.InsertSessionNotice(ctx, InsertSessionNoticeParams{
		Owner: owner, Kind: "supervision", Text: text, Wake: 1,
		CreatedAt: 1700000000, UpdatedAt: 1700000000,
	})
	require.NoError(t, err)
	_, err = conn2(t, q).ExecContext(ctx, `UPDATE session_notices SET delivery = 'done' WHERE id = ?`, n.ID)
	require.NoError(t, err)
	return n.ID
}

func TestIncrementSessionNoticeWakeAttempts_ScopedToIDSet(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)

	id1 := makeDoneSessionNotice(t, ctx, q, "sess-1", "n1")
	id2 := makeDoneSessionNotice(t, ctx, q, "sess-1", "n2")

	rows, err := q.IncrementSessionNoticeWakeAttempts(ctx, IncrementSessionNoticeWakeAttemptsParams{
		UpdatedAt: 1700000002, Ids: []int64{id1},
	})
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows)

	n1, err := q.GetSessionNotice(ctx, id1)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n1.WakeAttempts)

	n2, err := q.GetSessionNotice(ctx, id2)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n2.WakeAttempts, "a notice outside the captured id set must not be incremented")
}

// TestSettleSessionNoticesReactedFailed_ScopedToIDSet is the session_notices
// half of TestSettleAsyncJobsReactedFailed_ScopedToIDSet: a notice that
// became 'done' after the failed turn's id set was captured must survive
// the settle untouched.
//
// REVERT CHECK: same procedure as the async_jobs test -- drop the
// `id IN (sqlc.slice('ids'))` scoping from SettleSessionNoticesReactedFailed
// in sql/session_notices.sql, regenerate, and this test's second notice
// assertion fails. Verified in this session; reverted back before commit.
func TestSettleSessionNoticesReactedFailed_ScopedToIDSet(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)

	id1 := makeDoneSessionNotice(t, ctx, q, "sess-1", "n1")
	id2 := makeDoneSessionNotice(t, ctx, q, "sess-1", "n2")

	rows, err := q.SettleSessionNoticesReactedFailed(ctx, SettleSessionNoticesReactedFailedParams{
		UpdatedAt: 1700000003, Ids: []int64{id1},
	})
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows)

	n1, err := q.GetSessionNotice(ctx, id1)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n1.Reacted)
	assert.EqualValues(t, 1, n1.ReactedFailed)

	n2, err := q.GetSessionNotice(ctx, id2)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n2.Reacted, "a notice outside the captured id set must not be settled")
	assert.EqualValues(t, 0, n2.ReactedFailed)
}

func TestListReactedFailedSessionNoticesForOwner(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)

	id1 := makeDoneSessionNotice(t, ctx, q, "sess-1", "n1")
	makeDoneSessionNotice(t, ctx, q, "sess-1", "n2")

	_, err := q.SettleSessionNoticesReactedFailed(ctx, SettleSessionNoticesReactedFailedParams{
		UpdatedAt: 1700000003, Ids: []int64{id1},
	})
	require.NoError(t, err)

	failed, err := q.ListReactedFailedSessionNoticesForOwner(ctx, "sess-1")
	require.NoError(t, err)
	require.Len(t, failed, 1)
	assert.Equal(t, "n1", failed[0].Text)
}
