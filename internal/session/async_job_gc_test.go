// Coverage for `sessions gc --jobs-older-than` (async_job_gc.go): the
// caller-chosen-age counterpart of PurgeExpired's fixed 7-day window.
// A5 (docs/reviews/2026-09-29-async-phase4-round1.md): both PurgeJobsOlderThan
// and CountJobsOlderThan must never touch a row that is still unreacted debt
// (wake=1, reacted=0), even once it is old and delivered -- previously
// missing from this file's coverage entirely (no test existed for either
// method).
package session

import (
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
