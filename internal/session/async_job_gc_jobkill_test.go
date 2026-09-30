// R5A-1: retention must not delete a job_kill row whose result message was
// never named (done, notice_message_id NULL): dead-host recovery re-pends it.
package session

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestPurgeJobsOlderThan_KeepsUnnamedJobKillRowForRecovery: an old job_kill
// row that never got its fused result write survives PurgeJobsOlderThan and
// PurgeExpired (the count agrees), and RecoverDeadHost then re-pends it. A
// named job_kill row and an ordinary done row are still purged (control).
//
// Revert-check: drop `AND NOT (delivery = 'done' AND notice_kind = 'job_kill'
// AND notice_message_id IS NULL)` from PurgeAsyncJobsOlderThan -> the row is
// purged and Repended is 0.
func TestPurgeJobsOlderThan_KeepsUnnamedJobKillRowForRecovery(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	const dead = "dead-host-r5a1"
	fabricateDeadHost(t, ctx, q, store.dataDir, dead)

	for _, id := range []string{"call-unnamed", "call-named"} {
		seedRunningJob(t, ctx, q, "owner-1", id, dead, "", true)
		res, err := store.Transition(ctx, TransitionParams{
			Owner: "owner-1", ToolCallID: id, State: "cancelled", NoticeKind: "job_kill",
			ResultSummary: "partial output of " + id, Delivery: "done", Reacted: true,
		})
		require.NoError(t, err)
		require.Equal(t, TransitionWon, res.Outcome)
	}
	seedRunningJob(t, ctx, q, "owner-1", "call-plain", dead, "", true)
	_, err := store.Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "call-plain", State: "completed", ResultSummary: "ok",
		Delivery: "done", Reacted: true,
	})
	require.NoError(t, err)
	_, err = store.sqlDB.ExecContext(ctx,
		`UPDATE async_jobs SET notice_message_id = 'm-named' WHERE tool_call_id IN ('call-named', 'call-plain')`)
	require.NoError(t, err)
	old := time.Now().Add(-30 * 24 * time.Hour).Unix()
	_, err = store.sqlDB.ExecContext(ctx, `UPDATE async_jobs SET updated_at = ?`, old)
	require.NoError(t, err)

	counted, _, err := store.CountJobsOlderThan(ctx, time.Hour)
	require.NoError(t, err)
	require.EqualValues(t, 2, counted, "the dry-run count keeps the unnamed job_kill row too")
	purged, _, err := store.PurgeJobsOlderThan(ctx, time.Hour)
	require.NoError(t, err)
	require.EqualValues(t, 2, purged)
	require.NoError(t, store.PurgeExpired(ctx, time.Hour))

	_, err = store.Get(ctx, "owner-1", "call-named")
	require.ErrorIs(t, err, sql.ErrNoRows, "a job_kill row that names its result message is ordinary history")
	_, err = store.Get(ctx, "owner-1", "call-plain")
	require.ErrorIs(t, err, sql.ErrNoRows)
	kept, err := store.Get(ctx, "owner-1", "call-unnamed")
	require.NoError(t, err, "an unnamed job_kill row is what recovery repairs; retention must leave it")
	require.Equal(t, "done", kept.Delivery)

	out, err := store.RecoverDeadHost(ctx, dead, nil)
	require.NoError(t, err)
	require.Equal(t, 1, out.Repended, "recovery must still find the row to re-pend")
	got, err := store.Get(ctx, "owner-1", "call-unnamed")
	require.NoError(t, err)
	require.Equal(t, "pending", got.Delivery)
}
