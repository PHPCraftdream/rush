package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// conn2 recovers the *sql.DB a *Queries was built from, for the raw UPDATEs
// the helpers need (moving a notice straight to delivery='done' without a
// message insert has no generated query).
func conn2(t *testing.T, q *Queries) *sql.DB {
	t.Helper()
	c, ok := q.db.(*sql.DB)
	require.True(t, ok, "test helper expects a *Queries built directly over a *sql.DB")
	return c
}

// makeDoneSessionNotice inserts a wake notice and moves it straight to the
// delivered state carrying notice message "msg-<text>" -- the shape a
// settle-by-failure pass's debt snapshot is built from.
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
	ctx, conn := setupStoreV1DB(t)
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

// TestSettleSessionNoticeReactedFailedForSnapshotRow_OnlyTheSnapshotRow pins
// the settle guard: only the notice the snapshot named is settled, and only
// while it is still in the delivered state.
func TestSettleSessionNoticeReactedFailedForSnapshotRow_OnlyTheSnapshotRow(t *testing.T) {
	ctx, conn := setupStoreV1DB(t)
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
	ctx, conn := setupStoreV1DB(t)
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
