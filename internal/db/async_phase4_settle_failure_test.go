package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeDoneAsyncJob claims and terminal-transitions a job to a
// wake=1/delivery=done/reacted=0/announced=1 state carrying notice message
// "msg-<toolCallID>" -- the shape a settle-by-failure pass's debt snapshot is
// built from (doc sec.3.4: "only for the rows that were done at the moment
// [the failed turn] started"). It returns the row's claim id.
func makeDoneAsyncJob(t *testing.T, ctx context.Context, q *Queries, owner, toolCallID, hostID string) string {
	t.Helper()
	claimID := "claim-" + toolCallID
	_, err := q.ClaimAsyncJob(ctx, ClaimAsyncJobParams{
		OwnerSessionID: owner, ToolCallID: toolCallID, Kind: "command",
		InputHash: "h", HostID: hostID, ClaimID: claimID, CreatedAt: 1700000000, UpdatedAt: 1700000000,
	})
	require.NoError(t, err)
	_, err = q.MarkAsyncJobAnnounced(ctx, MarkAsyncJobAnnouncedParams{
		UpdatedAt: 1700000000, OwnerSessionID: owner, ToolCallID: toolCallID,
	})
	require.NoError(t, err)
	_, err = q.TransitionAsyncJobTerminalPreserveVoid(ctx, TransitionAsyncJobTerminalPreserveVoidParams{
		State: "completed", NoticeKind: "", ResultSummary: sql.NullString{String: "ok", Valid: true},
		ResultIsError: sql.NullInt64{Int64: 0, Valid: true}, Wake: 1, UpdatedAt: 1700000001,
		Delivery:       "pending",
		OwnerSessionID: owner, ToolCallID: toolCallID, ClaimID: claimID,
	})
	require.NoError(t, err)
	// Terminal transition lands delivery='pending'; the drain pulls it to
	// 'done' (naming the notice message) before a turn ever sees it --
	// settle-by-failure only touches rows that were 'done', so move it there
	// directly for the test.
	_, err = conn2(t, q).ExecContext(ctx,
		`UPDATE async_jobs SET delivery = 'done', notice_message_id = ? WHERE owner_session_id = ? AND tool_call_id = ?`,
		"msg-"+toolCallID, owner, toolCallID)
	require.NoError(t, err)
	return claimID
}

// conn2 recovers the *sql.DB a *Queries was built from, for the raw UPDATEs
// the helpers need (there is no PullPendingAsyncJobNotice-free way to move a
// row straight to delivery='done' without a message insert).
func conn2(t *testing.T, q *Queries) *sql.DB {
	t.Helper()
	c, ok := q.db.(*sql.DB)
	require.True(t, ok, "test helper expects a *Queries built directly over a *sql.DB")
	return c
}

func snapshotIncrement(claimID, toolCallID, msgID string) IncrementAsyncJobWakeAttemptsForSnapshotRowParams {
	return IncrementAsyncJobWakeAttemptsForSnapshotRowParams{
		UpdatedAt: 1700000002, Owner: "sess-1", ClaimID: claimID, ToolCallID: toolCallID, NoticeMessageID: msgID,
	}
}

func snapshotSettle(claimID, toolCallID, msgID string) SettleAsyncJobReactedFailedForSnapshotRowParams {
	return SettleAsyncJobReactedFailedForSnapshotRowParams{
		UpdatedAt: 1700000003, Owner: "sess-1", ClaimID: claimID, ToolCallID: toolCallID, NoticeMessageID: msgID,
	}
}

// TestIncrementAsyncJobWakeAttemptsForSnapshotRow_OnlyTheSnapshotRow pins the
// per-row guard (R2A-4/R2A-5): the increment lands on the row the snapshot
// saw and on nothing else -- not a sibling, not the same tool_call_id under a
// different claim, not a row re-pended (new notice message) or voided since.
//
// REVERT CHECK: dropping `AND claim_id = @claim_id`, the notice_message_id
// comparison or `delivery = 'done'` from the query (sql/async_jobs.sql,
// regenerated) turns the matching case of the loop below (or the void case)
// into 1 affected row and the test fails.
func TestIncrementAsyncJobWakeAttemptsForSnapshotRow_OnlyTheSnapshotRow(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)
	_, err := q.RegisterAsyncHost(ctx, RegisterAsyncHostParams{ID: "host-1", Pid: 1, StartedAt: 1700000000})
	require.NoError(t, err)

	claim1 := makeDoneAsyncJob(t, ctx, q, "sess-1", "call-1", "host-1")
	makeDoneAsyncJob(t, ctx, q, "sess-1", "call-2", "host-1")

	// Stale references first: they must change nothing.
	for name, p := range map[string]IncrementAsyncJobWakeAttemptsForSnapshotRowParams{
		"a different claim under the same tool_call_id": snapshotIncrement("claim-other", "call-1", "msg-call-1"),
		"a re-pulled row (different notice message)":    snapshotIncrement(claim1, "call-1", "msg-other"),
	} {
		rows, err := q.IncrementAsyncJobWakeAttemptsForSnapshotRow(ctx, p)
		require.NoError(t, err, name)
		assert.EqualValues(t, 0, rows, name)
	}

	rows, err := q.IncrementAsyncJobWakeAttemptsForSnapshotRow(ctx, snapshotIncrement(claim1, "call-1", "msg-call-1"))
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows)

	j1, err := q.GetAsyncJob(ctx, GetAsyncJobParams{OwnerSessionID: "sess-1", ToolCallID: "call-1"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, j1.WakeAttempts)
	j2, err := q.GetAsyncJob(ctx, GetAsyncJobParams{OwnerSessionID: "sess-1", ToolCallID: "call-2"})
	require.NoError(t, err)
	assert.EqualValues(t, 0, j2.WakeAttempts, "a row outside the snapshot must not be incremented")

	_, err = conn2(t, q).ExecContext(ctx, `UPDATE async_jobs SET delivery = 'void' WHERE owner_session_id = 'sess-1' AND tool_call_id = 'call-1'`)
	require.NoError(t, err)
	rows, err = q.IncrementAsyncJobWakeAttemptsForSnapshotRow(ctx, snapshotIncrement(claim1, "call-1", "msg-call-1"))
	require.NoError(t, err)
	assert.EqualValues(t, 0, rows, "a voided row must not be incremented")
}

// TestSettleAsyncJobReactedFailedForSnapshotRow_OnlyTheSnapshotRow is the
// settle twin: doc sec.3.4 "only for rows that were done at the moment the
// turn started" -- and still the same rows, not whatever now carries the
// tool_call_id text or a re-pended delivery.
//
// REVERT CHECK: same procedure as the increment test on
// SettleAsyncJobReactedFailedForSnapshotRow.
func TestSettleAsyncJobReactedFailedForSnapshotRow_OnlyTheSnapshotRow(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)
	_, err := q.RegisterAsyncHost(ctx, RegisterAsyncHostParams{ID: "host-1", Pid: 1, StartedAt: 1700000000})
	require.NoError(t, err)

	claim1 := makeDoneAsyncJob(t, ctx, q, "sess-1", "call-1", "host-1")
	makeDoneAsyncJob(t, ctx, q, "sess-1", "call-2", "host-1")

	// Stale references settle nothing.
	for name, p := range map[string]SettleAsyncJobReactedFailedForSnapshotRowParams{
		"a different claim under the same tool_call_id": snapshotSettle("claim-other", "call-1", "msg-call-1"),
		"a re-pulled row (different notice message)":    snapshotSettle(claim1, "call-1", "msg-other"),
	} {
		rows, err := q.SettleAsyncJobReactedFailedForSnapshotRow(ctx, p)
		require.NoError(t, err, name)
		assert.EqualValues(t, 0, rows, name)
	}
	_, err = conn2(t, q).ExecContext(ctx, `UPDATE async_jobs SET delivery = 'pending' WHERE owner_session_id = 'sess-1' AND tool_call_id = 'call-1'`)
	require.NoError(t, err)
	rows, err := q.SettleAsyncJobReactedFailedForSnapshotRow(ctx, snapshotSettle(claim1, "call-1", "msg-call-1"))
	require.NoError(t, err)
	assert.EqualValues(t, 0, rows, "a row re-pended since the snapshot must not be settled")
	_, err = conn2(t, q).ExecContext(ctx, `UPDATE async_jobs SET delivery = 'done' WHERE owner_session_id = 'sess-1' AND tool_call_id = 'call-1'`)
	require.NoError(t, err)

	rows, err = q.SettleAsyncJobReactedFailedForSnapshotRow(ctx, snapshotSettle(claim1, "call-1", "msg-call-1"))
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows)

	j1, err := q.GetAsyncJob(ctx, GetAsyncJobParams{OwnerSessionID: "sess-1", ToolCallID: "call-1"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, j1.Reacted)
	assert.EqualValues(t, 1, j1.ReactedFailed)

	j2, err := q.GetAsyncJob(ctx, GetAsyncJobParams{OwnerSessionID: "sess-1", ToolCallID: "call-2"})
	require.NoError(t, err)
	assert.EqualValues(t, 0, j2.Reacted, "a row outside the snapshot must not be settled")
	assert.EqualValues(t, 0, j2.ReactedFailed)

	debt, err := q.AsyncReactionDebtExists(ctx, "sess-1")
	require.NoError(t, err)
	assert.True(t, debt.Bool, "call-2's own debt must survive the settle untouched")
}

func TestListReactedFailedAsyncJobsForOwner(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)
	_, err := q.RegisterAsyncHost(ctx, RegisterAsyncHostParams{ID: "host-1", Pid: 1, StartedAt: 1700000000})
	require.NoError(t, err)

	claim1 := makeDoneAsyncJob(t, ctx, q, "sess-1", "call-1", "host-1")
	makeDoneAsyncJob(t, ctx, q, "sess-1", "call-2", "host-1")

	_, err = q.SettleAsyncJobReactedFailedForSnapshotRow(ctx, snapshotSettle(claim1, "call-1", "msg-call-1"))
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
	_, err = conn2(t, q).ExecContext(ctx, `UPDATE session_notices SET delivery = 'done', notice_message_id = ? WHERE id = ?`, "msg-"+text, n.ID)
	require.NoError(t, err)
	return n.ID
}

func TestIncrementSessionNoticeWakeAttemptsForSnapshotRow_OnlyTheSnapshotRow(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)

	id1 := makeDoneSessionNotice(t, ctx, q, "sess-1", "n1")
	id2 := makeDoneSessionNotice(t, ctx, q, "sess-1", "n2")

	rows, err := q.IncrementSessionNoticeWakeAttemptsForSnapshotRow(ctx, IncrementSessionNoticeWakeAttemptsForSnapshotRowParams{
		UpdatedAt: 1700000002, ID: id1, NoticeMessageID: "msg-other",
	})
	require.NoError(t, err)
	assert.EqualValues(t, 0, rows, "a re-pulled notice (different message) must not be incremented")

	rows, err = q.IncrementSessionNoticeWakeAttemptsForSnapshotRow(ctx, IncrementSessionNoticeWakeAttemptsForSnapshotRowParams{
		UpdatedAt: 1700000002, ID: id1, NoticeMessageID: "msg-n1",
	})
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows)

	n1, err := q.GetSessionNotice(ctx, id1)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n1.WakeAttempts)
	n2, err := q.GetSessionNotice(ctx, id2)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n2.WakeAttempts, "a notice outside the snapshot must not be incremented")
}

// TestSettleSessionNoticeReactedFailedForSnapshotRow_OnlyTheSnapshotRow is the
// session_notices half of the async_jobs settle test.
func TestSettleSessionNoticeReactedFailedForSnapshotRow_OnlyTheSnapshotRow(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)

	id1 := makeDoneSessionNotice(t, ctx, q, "sess-1", "n1")
	id2 := makeDoneSessionNotice(t, ctx, q, "sess-1", "n2")

	rows, err := q.SettleSessionNoticeReactedFailedForSnapshotRow(ctx, SettleSessionNoticeReactedFailedForSnapshotRowParams{
		UpdatedAt: 1700000003, ID: id1, NoticeMessageID: "msg-other",
	})
	require.NoError(t, err)
	assert.EqualValues(t, 0, rows, "a re-pulled notice (different message) must not be settled")

	rows, err = q.SettleSessionNoticeReactedFailedForSnapshotRow(ctx, SettleSessionNoticeReactedFailedForSnapshotRowParams{
		UpdatedAt: 1700000003, ID: id1, NoticeMessageID: "msg-n1",
	})
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows)

	n1, err := q.GetSessionNotice(ctx, id1)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n1.Reacted)
	assert.EqualValues(t, 1, n1.ReactedFailed)

	n2, err := q.GetSessionNotice(ctx, id2)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n2.Reacted, "a notice outside the snapshot must not be settled")
	assert.EqualValues(t, 0, n2.ReactedFailed)
}

func TestListReactedFailedSessionNoticesForOwner(t *testing.T) {
	ctx, conn := setupPhase4DB(t)
	q := New(conn)

	id1 := makeDoneSessionNotice(t, ctx, q, "sess-1", "n1")
	makeDoneSessionNotice(t, ctx, q, "sess-1", "n2")

	_, err := q.SettleSessionNoticeReactedFailedForSnapshotRow(ctx, SettleSessionNoticeReactedFailedForSnapshotRowParams{
		UpdatedAt: 1700000003, ID: id1, NoticeMessageID: "msg-n1",
	})
	require.NoError(t, err)

	failed, err := q.ListReactedFailedSessionNoticesForOwner(ctx, "sess-1")
	require.NoError(t, err)
	require.Len(t, failed, 1)
	assert.Equal(t, "n1", failed[0].Text)
}
