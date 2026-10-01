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

	hasJobDebt, rows, _, err := store.PendingInclusiveDebtRows(ctx, "owner-1")
	require.NoError(t, err)
	require.False(t, hasJobDebt)
	require.Equal(t, []PendingNoticeDebt{{ID: owed, Kind: NoticeKindBGShellDone}, {ID: last, Kind: NoticeKindSupervision}}, rows)

	_, err = store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "completed", Wake: true})
	require.NoError(t, err)
	hasJobDebt, _, claims, err := store.PendingInclusiveDebtRows(ctx, "owner-1")
	require.NoError(t, err)
	require.True(t, hasJobDebt, "an announced, unreacted wake=1 job row is job debt")
	require.Len(t, claims, 1, "the debt job row's claim id is named")
	require.NotEmpty(t, claims[0])
}

// Job debt is announced, wake=1, unreacted and not void; a job row failing any
// one of those is NOT debt. Each row below keeps every other condition true, so
// dropping exactly one predicate in PendingInclusiveDebtRows turns exactly that
// case red (a settled or voided job would otherwise report debt forever and
// the web bg-shell cap, ASYNC-09, would never defer).
//
// Revert-check: dropping `j.Wake != 0`, `j.Reacted == 0`,
// `j.Delivery != "void"` or `j.Announced != 0` from the job loop fails the
// matching case below.
func TestPendingInclusiveDebtRows_JobDebtExclusions(t *testing.T) {
	cases := []struct {
		name      string
		announce  bool
		wake      bool
		setSQL    string // applied after the transition; "" = leave the row as is
		wantDebts bool
	}{
		{name: "announced_unreacted_wake_is_debt", announce: true, wake: true, wantDebts: true},
		{name: "reacted_job_is_settled", announce: true, wake: true, setSQL: `UPDATE async_jobs SET reacted=1, delivery='done'`},
		{name: "void_job_is_not_debt", announce: true, wake: true, setSQL: `UPDATE async_jobs SET delivery='void'`},
		{name: "unannounced_terminal_job_is_not_debt", announce: false, wake: true},
		{name: "wake_zero_job_is_not_debt", announce: true, wake: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, q, ctx := newTestStore(t)
			require.NoError(t, seedSession(ctx, q, "owner-1"))
			_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
			require.NoError(t, err)
			if tc.announce {
				require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))
			}
			_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "completed", Wake: tc.wake})
			require.NoError(t, err)
			if tc.setSQL != "" {
				_, err = store.sqlDB.ExecContext(ctx, tc.setSQL+` WHERE owner_session_id='owner-1'`)
				require.NoError(t, err)
			}

			hasJobDebt, notices, _, err := store.PendingInclusiveDebtRows(ctx, "owner-1")
			require.NoError(t, err)
			require.Equal(t, tc.wantDebts, hasJobDebt)
			require.Empty(t, notices, "no notice row was inserted")
		})
	}
}

// A notice the driver already pulled (delivery='done') but that is still
// unreacted is debt exactly like a 'pending' one, and is named in the result.
//
// Revert-check: narrowing the notice filter to `n.Delivery == "pending"` drops
// the done row and the assertion goes red.
func TestPendingInclusiveDebtRows_IncludesDoneUnreactedNotice(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	pending, err := store.InsertSessionNoticeReturningID(ctx, "owner-1", NoticeKindBGShellDone, "pending", true, "")
	require.NoError(t, err)
	done, err := store.InsertSessionNoticeReturningID(ctx, "owner-1", NoticeKindBGShellDone, "done", true, "")
	require.NoError(t, err)
	_, err = store.sqlDB.ExecContext(ctx, `UPDATE session_notices SET delivery='done' WHERE id=?`, done)
	require.NoError(t, err)

	hasJobDebt, rows, _, err := store.PendingInclusiveDebtRows(ctx, "owner-1")
	require.NoError(t, err)
	require.False(t, hasJobDebt)
	require.Equal(t, []PendingNoticeDebt{{ID: pending, Kind: NoticeKindBGShellDone}, {ID: done, Kind: NoticeKindBGShellDone}}, rows)
}
