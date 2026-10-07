// Abort vs a Stop-cancelled durable row (#1266): DeleteUnannouncedAsyncJob is
// scoped to announced=0 AND state<>'cancelled', so abort must not erase a row
// that Stop (cancelSession) already committed as 'cancelled', yet it still
// drops the unannounced job from memory (no orphan holding a ledger slot);
// an unannounced still-running row is still cleaned up as ASYNC-05 requires.
//
// Revert-checks: dropping the state condition from DeleteUnannouncedAsyncJob
// deletes the Stop-cancelled row in (a); restoring abort's early return on any
// surviving row keeps the job in memory in (a).
package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWorkLedger_AbortKeepsStopCancelledRow covers (a): cancelSession commits
// the row as 'cancelled' with announced=0; the subsequent failed "started"
// write path's abort must keep the durable row and drop the in-memory job.
func TestWorkLedger_AbortKeepsStopCancelledRow(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	l := newWorkLedger(nil)
	l.store = store
	ctx := context.Background()

	_, _, err := l.Start("owner-1", "call-1", "sleep 100", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	job := jobOf(l, "owner-1", "call-1")
	// The "started" write has not been persisted, so the job is unannounced:
	// exactly the pre-abort state the failed "started" write produces.

	l.cancelSession("owner-1")
	row, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "cancelled", row.State)
	require.EqualValues(t, 0, row.Announced, "cancelSession commits announced=0 for an unannounced job")

	// Simulate the failed "started" tool-result write path calling abort.
	l.abort(job)

	row2, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err, "abort must not delete the Stop-cancelled durable row")
	require.Equal(t, "cancelled", row2.State)

	l.mu.Lock()
	_, present := l.bySession["owner-1"].jobs["call-1"]
	l.mu.Unlock()
	require.False(t, present, "abort must drop the unannounced job even though its cancelled row survives")
	require.False(t, l.running("owner-1"), "no orphan may keep the session running")
}

// TestWorkLedger_AbortStillDeletesUnannouncedRunningRow covers (b) ASYNC-05:
// an unannounced row still in state='running' is deleted by abort and the
// in-memory job is dropped.
func TestWorkLedger_AbortStillDeletesUnannouncedRunningRow(t *testing.T) {
	t.Parallel()
	store := newTestAsyncJobStore(t)
	l := newWorkLedger(nil)
	l.store = store

	_, _, err := l.Start("owner-1", "call-1", "echo hi", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	job := jobOf(l, "owner-1", "call-1")
	// Unannounced: the "started" write never happened on this abort path.

	row, err := store.Get(context.Background(), "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "running", row.State)
	require.EqualValues(t, 0, row.Announced)

	l.abort(job)

	_, err = store.Get(context.Background(), "owner-1", "call-1")
	require.Error(t, err, "abort must delete an unannounced still-running row (ASYNC-05)")

	l.mu.Lock()
	_, present := l.bySession["owner-1"].jobs["call-1"]
	l.mu.Unlock()
	require.False(t, present, "abort must drop the in-memory job when the row is gone")
}
