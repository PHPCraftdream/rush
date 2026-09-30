// Coverage for `sessions gc --jobs-older-than` (async_job_gc.go): the
// caller-chosen-age counterpart of PurgeExpired's fixed 7-day window.
// A5 (docs/reviews/2026-09-29-async-phase4-round1.md): both PurgeJobsOlderThan
// and CountJobsOlderThan must never touch a row that is still unreacted debt
// (wake=1, reacted=0), even once it is old and delivered -- previously
// missing from this file's coverage entirely (no test existed for either
// method).
package session

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestPurgeJobsOlderThan_NeverPurgesUnreactedDebt(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	old := time.Now().Add(-30 * 24 * time.Hour).Unix()

	// Old, delivered, reacted -- ordinary history, must be purged.
	_, err := q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
		OwnerSessionID: "owner-1", ToolCallID: "old-reacted", Kind: "command",
		InputHash: "h1", HostID: "gone-host", CreatedAt: old, UpdatedAt: old,
	})
	require.NoError(t, err)
	_, err = q.TransitionAsyncJobTerminalPreserveVoid(ctx, db.TransitionAsyncJobTerminalPreserveVoidParams{
		State: "completed", Delivery: "done", ResultSummary: sql.NullString{String: "x", Valid: true},
		ResultIsError: sql.NullInt64{Valid: true}, Wake: 1, Reacted: 1, UpdatedAt: old,
		OwnerSessionID: "owner-1", ToolCallID: "old-reacted",
	})
	require.NoError(t, err)

	// Old, delivered, STILL UNREACTED (wake=1, reacted=0) -- must survive.
	_, err = q.ClaimAsyncJob(ctx, db.ClaimAsyncJobParams{
		OwnerSessionID: "owner-1", ToolCallID: "old-debt", Kind: "command",
		InputHash: "h2", HostID: "gone-host", CreatedAt: old, UpdatedAt: old,
	})
	require.NoError(t, err)
	_, err = q.TransitionAsyncJobTerminalPreserveVoid(ctx, db.TransitionAsyncJobTerminalPreserveVoidParams{
		State: "completed", Delivery: "done", ResultSummary: sql.NullString{String: "y", Valid: true},
		ResultIsError: sql.NullInt64{Valid: true}, Wake: 1, Reacted: 0, UpdatedAt: old,
		OwnerSessionID: "owner-1", ToolCallID: "old-debt",
	})
	require.NoError(t, err)

	jobs, notices, err := store.CountJobsOlderThan(ctx, time.Hour)
	require.NoError(t, err)
	require.EqualValues(t, 1, jobs, "only the reacted row counts as purgeable")
	require.EqualValues(t, 0, notices)

	jobs, notices, err = store.PurgeJobsOlderThan(ctx, time.Hour)
	require.NoError(t, err)
	require.EqualValues(t, 1, jobs)
	require.EqualValues(t, 0, notices)

	_, err = store.Get(ctx, "owner-1", "old-reacted")
	require.ErrorIs(t, err, sql.ErrNoRows, "the reacted row must be purged")
	_, err = store.Get(ctx, "owner-1", "old-debt")
	require.NoError(t, err, "unreacted debt must survive an operator-triggered purge too")
}

func TestPurgeSessionNoticesOlderThan_NeverPurgesUnreactedDebt(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	old := time.Now().Add(-30 * 24 * time.Hour).Unix()
	_, err := q.InsertSessionNotice(ctx, db.InsertSessionNoticeParams{
		Owner: "owner-1", Kind: "supervision", Text: "n", Wake: 1,
		CreatedAt: old, UpdatedAt: old,
	})
	require.NoError(t, err)
	notices, err := q.ListSessionNoticesForOwner(ctx, "owner-1")
	require.NoError(t, err)
	require.Len(t, notices, 1)
	// Move it to delivery='done', reacted=0 (unreacted debt), still old.
	_, err = q.PullPendingSessionNotice(ctx, db.PullPendingSessionNoticeParams{UpdatedAt: old, ID: notices[0].ID})
	require.NoError(t, err)

	jobs, count, err := store.CountJobsOlderThan(ctx, time.Hour)
	require.NoError(t, err)
	require.EqualValues(t, 0, jobs)
	require.EqualValues(t, 0, count, "an old, unreacted notice must not count as purgeable")

	_, deleted, err := store.PurgeJobsOlderThan(ctx, time.Hour)
	require.NoError(t, err)
	require.EqualValues(t, 0, deleted)

	got, err := q.GetSessionNotice(ctx, notices[0].ID)
	require.NoError(t, err)
	require.EqualValues(t, notices[0].ID, got.ID, "unreacted debt notice must survive")
}

// seedFinishedDelegation inserts a terminal, delivered, reacted delegation row
// owned by parent for child, old enough for any retention cutoff.
func seedFinishedDelegation(t *testing.T, ctx context.Context, store *AsyncJobStore, q *db.Queries, parent, child, toolCallID string) {
	t.Helper()
	seedRunningJob(t, ctx, q, parent, toolCallID, store.HostID(), child, true)
	_, err := store.Transition(ctx, TransitionParams{Owner: parent, ToolCallID: toolCallID, State: "completed", ResultSummary: "ok", Wake: true})
	require.NoError(t, err)
	_, err = store.sqlDB.ExecContext(ctx,
		`UPDATE async_jobs SET delivery = 'done', reacted = 1, notice_message_id = 'm', updated_at = 100 WHERE owner_session_id = ? AND tool_call_id = ?`,
		parent, toolCallID)
	require.NoError(t, err)
}

// TestPurgeJobsOlderThan_KeepsDelegationRowWhileChildStillHasWork is R2A-10:
// isDurableDelegationChild recognises a released delegation child only by the
// parent's delegation row, so retention must not delete that row while the
// child still has a running row or unreacted debt (async_jobs or
// session_notices, pending included) -- otherwise the child later gets an
// uncapped Drain on the parent's agent. Once the child is idle the row is
// purged as before, and the dry-run count agrees with the purge.
//
// REVERT CHECK: the child-work predicate removed from PurgeAsyncJobsOlderThan
// and CountAsyncJobsOlderThan (sql/async_jobs.sql, regenerated) -- every
// "kept" sub-case purged its delegation row and the require.Zero below failed.
// Restored; re-ran, passed.
func TestPurgeJobsOlderThan_KeepsDelegationRowWhileChildStillHasWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		child func(t *testing.T, ctx context.Context, store *AsyncJobStore, q *db.Queries)
		kept  bool
	}{
		{"child has a running row", func(t *testing.T, ctx context.Context, store *AsyncJobStore, q *db.Queries) {
			seedRunningJob(t, ctx, q, "child-1", "child-job", store.HostID(), "", true)
		}, true},
		{"child has an unreacted done row", func(t *testing.T, ctx context.Context, store *AsyncJobStore, q *db.Queries) {
			seedRunningJob(t, ctx, q, "child-1", "child-job", store.HostID(), "", true)
			_, err := store.Transition(ctx, TransitionParams{Owner: "child-1", ToolCallID: "child-job", State: "completed", ResultSummary: "ok", Wake: true})
			require.NoError(t, err)
			_, err = store.sqlDB.ExecContext(ctx, `UPDATE async_jobs SET delivery = 'done', notice_message_id = 'm' WHERE owner_session_id = 'child-1'`)
			require.NoError(t, err)
		}, true},
		{"child has a pending unreacted row", func(t *testing.T, ctx context.Context, store *AsyncJobStore, q *db.Queries) {
			seedRunningJob(t, ctx, q, "child-1", "child-job", store.HostID(), "", true)
			_, err := store.Transition(ctx, TransitionParams{Owner: "child-1", ToolCallID: "child-job", State: "completed", ResultSummary: "ok", Wake: true})
			require.NoError(t, err)
		}, true},
		{"child has a pending notice", func(t *testing.T, ctx context.Context, store *AsyncJobStore, q *db.Queries) {
			require.NoError(t, store.InsertSessionNotice(ctx, "child-1", NoticeKindBGShellDone, "bg", true, ""))
		}, true},
		{"child is idle", func(t *testing.T, ctx context.Context, store *AsyncJobStore, q *db.Queries) {}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, q, ctx := newTestStore(t)
			require.NoError(t, seedSession(ctx, q, "parent-1"))
			require.NoError(t, seedSession(ctx, q, "child-1"))
			// Register the store's host first so seeded rows sit on a live host.
			_, err := store.ensureHost(ctx)
			require.NoError(t, err)
			seedFinishedDelegation(t, ctx, store, q, "parent-1", "child-1", "deleg-1")
			tc.child(t, ctx, store, q)

			wantJobs := int64(0)
			if !tc.kept {
				wantJobs = 1
			}
			counted, _, err := store.CountJobsOlderThan(ctx, time.Second)
			require.NoError(t, err)
			require.Equal(t, wantJobs, counted, "the dry-run count must apply the same predicate as the purge")
			purged, _, err := store.PurgeJobsOlderThan(ctx, time.Second)
			require.NoError(t, err)
			require.Equal(t, wantJobs, purged)

			_, getErr := store.Get(ctx, "parent-1", "deleg-1")
			if tc.kept {
				require.NoError(t, getErr, "the delegation row must survive while its child still has work")
			} else {
				require.ErrorIs(t, getErr, sql.ErrNoRows, "an idle child's delegation row is purged as before")
			}
		})
	}
}
