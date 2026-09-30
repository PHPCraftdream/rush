package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// InsertSessionNoticeReturningID returns the id of the row it created (the one
// ListSessionNotices reports), and a failed insert returns no id.
//
// Revert-check: returning a constant id (or the zero value) turns the id
// assertions red.
func TestInsertSessionNoticeReturningID_ReturnsTheNewRowID(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	first, err := store.InsertSessionNoticeReturningID(ctx, "owner-1", NoticeKindBGShellDone, "one", true, "")
	require.NoError(t, err)
	second, err := store.InsertSessionNoticeReturningID(ctx, "owner-1", NoticeKindBGShellDone, "two", true, "")
	require.NoError(t, err)
	require.Greater(t, second, first)

	rows, err := store.ListSessionNotices(ctx, "owner-1")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, first, rows[0].ID)
	require.Equal(t, second, rows[1].ID)

	// A session that does not exist violates the foreign key: no row, no id.
	id, err := store.InsertSessionNoticeReturningID(ctx, "no-such-session", NoticeKindBGShellDone, "x", true, "")
	require.Error(t, err)
	require.Zero(t, id)
}

// PendingInclusiveDebtRows names the notice rows of the debt (wake=1,
// reacted=0, not void; pending or done), oldest first, with their kinds, and
// reports job debt (announced, wake=1, unreacted) like the summary.
//
// Revert-check: dropping the reacted filter, the wake filter or the void filter
// puts a settled/silent/void row into the result and turns the id assertion red.
func TestPendingInclusiveDebtRows_NamesTheDebtNoticeRows(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	owed, err := store.InsertSessionNoticeReturningID(ctx, "owner-1", NoticeKindBGShellDone, "owed", true, "")
	require.NoError(t, err)
	_, err = store.InsertSessionNoticeReturningID(ctx, "owner-1", NoticeKindSupervision, "silent", false, "")
	require.NoError(t, err)
	reacted, err := store.InsertSessionNoticeReturningID(ctx, "owner-1", NoticeKindBGShellDone, "reacted", true, "")
	require.NoError(t, err)
	voided, err := store.InsertSessionNoticeReturningID(ctx, "owner-1", NoticeKindBGShellDone, "void", true, "")
	require.NoError(t, err)
	last, err := store.InsertSessionNoticeReturningID(ctx, "owner-1", NoticeKindSupervision, "later", true, "")
	require.NoError(t, err)
	_, err = store.sqlDB.ExecContext(ctx, `UPDATE session_notices SET delivery='done', reacted=1 WHERE id=?`, reacted)
	require.NoError(t, err)
	_, err = store.sqlDB.ExecContext(ctx, `UPDATE session_notices SET delivery='void' WHERE id=?`, voided)
	require.NoError(t, err)

	hasJobDebt, rows, err := store.PendingInclusiveDebtRows(ctx, "owner-1")
	require.NoError(t, err)
	require.False(t, hasJobDebt)
	require.Equal(t, []PendingNoticeDebt{{ID: owed, Kind: NoticeKindBGShellDone}, {ID: last, Kind: NoticeKindSupervision}}, rows)

	_, err = store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "completed", Wake: true})
	require.NoError(t, err)
	hasJobDebt, _, err = store.PendingInclusiveDebtRows(ctx, "owner-1")
	require.NoError(t, err)
	require.True(t, hasJobDebt, "an announced, unreacted wake=1 job row is job debt")
}
