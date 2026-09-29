// RerunTruncate coverage (docs/plans/2026-09-28-async-phase4-durable-core.md
// sec.3.8, step 6): the DB-level reconciliation a Rerun truncation runs
// against async_jobs/session_notices -- void-by-tool-call-id, repend-by-
// notice-message-id, and the ordering rule that a row matching BOTH ends up
// void, never resurrected to pending.
package session

import (
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// TestRerunTruncate_VoidsRowByOwningToolCallID pins the first rule: any row
// (regardless of current delivery/state) whose owning tool call is in the
// deleted tail is forced to delivery='void'.
func TestRerunTruncate_VoidsRowByOwningToolCallID(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "echo hi"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "completed", Wake: true})
	require.NoError(t, err)
	row, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "pending", row.Delivery)

	require.NoError(t, store.RerunTruncate(ctx, "owner-1", []string{"call-1"}, nil))

	row, err = store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "void", row.Delivery)
}

// TestRerunTruncate_VoidsStillRunningRowAndLateTransitionPreservesIt proves
// the defense-in-depth half: a row still 'running' at truncation time (its
// stop raced or failed) is ALSO voided pre-emptively, and a late terminal
// transition (job_kill/natural finish arriving after) must preserve that
// void rather than resetting it to pending -- TransitionAsyncJobTerminalPreserveVoid
// is the ONLY terminal-transition query and already has this CASE.
func TestRerunTruncate_VoidsStillRunningRowAndLateTransitionPreservesIt(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "echo hi"})
	require.NoError(t, err)

	require.NoError(t, store.RerunTruncate(ctx, "owner-1", []string{"call-1"}, nil))
	row, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "running", row.State, "RerunTruncate must not itself stop the row -- that is the ledger's job")
	require.Equal(t, "void", row.Delivery)

	won, err := store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "cancelled", NoticeKind: "job_kill", Wake: false})
	require.NoError(t, err)
	require.Equal(t, TransitionWon, won.Outcome)
	require.Equal(t, "void", won.Row.Delivery, "a late terminal transition must preserve void, never resurrect it to pending")
}

// TestRerunTruncate_RependsRowByNoticeMessageID pins the second rule: a row
// already delivered (delivery='done') whose OWN notice message id is in the
// deleted tail goes back to pending/reacted=0 -- ASYNC-04 for the new
// branch.
func TestRerunTruncate_RependsRowByNoticeMessageID(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "echo hi"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "completed", Wake: true})
	require.NoError(t, err)

	// Simulate the pull having landed the notice at message id "msg-later"
	// (the pull itself lives in notice_pull.go; here only its DB effect on
	// the row -- delivery='done' + notice_message_id -- matters).
	_, err = q.PullPendingAsyncJobNotice(ctx, db.PullPendingAsyncJobNoticeParams{UpdatedAt: 2, OwnerSessionID: "owner-1", ToolCallID: "call-1"})
	require.NoError(t, err)
	_, err = q.SetAsyncJobNoticeMessageID(ctx, db.SetAsyncJobNoticeMessageIDParams{
		NoticeMessageID: sql.NullString{String: "msg-later", Valid: true}, UpdatedAt: 3, OwnerSessionID: "owner-1", ToolCallID: "call-1",
	})
	require.NoError(t, err)

	require.NoError(t, store.RerunTruncate(ctx, "owner-1", nil, []string{"msg-later"}))

	row, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "pending", row.Delivery, "the row must be re-pulled on the new branch")
	require.EqualValues(t, 0, row.Reacted)
}

// TestRerunTruncate_VoidWinsOverRependForTheSameRow pins the ordering rule
// (doc sec.3.8): a row whose OWN tool call AND whose earlier notice both
// fall in the deleted tail must end up void, not resurrected to pending by
// the repend step running first.
func TestRerunTruncate_VoidWinsOverRependForTheSameRow(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "echo hi"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "completed", Wake: true})
	require.NoError(t, err)
	_, err = q.PullPendingAsyncJobNotice(ctx, db.PullPendingAsyncJobNoticeParams{UpdatedAt: 2, OwnerSessionID: "owner-1", ToolCallID: "call-1"})
	require.NoError(t, err)
	_, err = q.SetAsyncJobNoticeMessageID(ctx, db.SetAsyncJobNoticeMessageIDParams{
		NoticeMessageID: sql.NullString{String: "msg-x", Valid: true}, UpdatedAt: 3, OwnerSessionID: "owner-1", ToolCallID: "call-1",
	})
	require.NoError(t, err)

	// BOTH the call AND its own notice are in the same deleted tail.
	require.NoError(t, store.RerunTruncate(ctx, "owner-1", []string{"call-1"}, []string{"msg-x"}))

	row, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "void", row.Delivery, "void must win when a row matches both the void and the repend condition")
}

// TestRerunTruncate_UnrelatedRowsUntouched pins the negative control: a row
// with no connection to either deleted-id set is left exactly as it was.
func TestRerunTruncate_UnrelatedRowsUntouched(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-unrelated", Kind: JobKindCommand, Input: "echo hi"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-unrelated"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-unrelated", State: "completed", Wake: true})
	require.NoError(t, err)

	require.NoError(t, store.RerunTruncate(ctx, "owner-1", []string{"call-deleted"}, []string{"msg-deleted"}))

	row, err := store.Get(ctx, "owner-1", "call-unrelated")
	require.NoError(t, err)
	require.Equal(t, "pending", row.Delivery, "an unrelated row must be untouched by an unrelated Rerun")
}

// TestRerunTruncate_SessionNoticesRependByMessageID mirrors
// TestRerunTruncate_RependsRowByNoticeMessageID for session_notices (doc
// sec.3.8: "same for session_notices rows whose messages were deleted").
func TestRerunTruncate_SessionNoticesRependByMessageID(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	require.NoError(t, store.InsertSessionNotice(ctx, "owner-1", NoticeKindSupervision, "check-in", true, ""))
	notices, err := store.ListSessionNotices(ctx, "owner-1")
	require.NoError(t, err)
	require.Len(t, notices, 1)
	id := notices[0].ID

	_, err = q.PullPendingSessionNotice(ctx, db.PullPendingSessionNoticeParams{UpdatedAt: 2, ID: id})
	require.NoError(t, err)
	_, err = q.SetSessionNoticeMessageID(ctx, db.SetSessionNoticeMessageIDParams{
		NoticeMessageID: sql.NullString{String: "msg-notice", Valid: true}, UpdatedAt: 3, ID: id,
	})
	require.NoError(t, err)

	require.NoError(t, store.RerunTruncate(ctx, "owner-1", nil, []string{"msg-notice"}))

	got, err := q.GetSessionNotice(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "pending", got.Delivery)
	require.EqualValues(t, 0, got.Reacted)
}
