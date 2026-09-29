// RerunTruncateAsyncJobs coverage (doc sec.3.8, step 6): the coordinator-
// level wiring between the ledger's Rerun-scoped stop and the store's
// Rerun DB reconciliation, end to end -- a LIVE task in the deleted tail
// ends with neither a pullable notice nor reaction debt.
package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func newRerunTestCoordinator(t *testing.T) *coordinator {
	t.Helper()
	store, _, _ := newTestAsyncJobStoreWithDataDir(t)
	coord := &coordinator{}
	coord.asyncJobs = newWorkLedger(nil)
	coord.asyncJobs.store = store
	coord.asyncJobs.coord = coord
	return coord
}

// TestCoordinator_RerunTruncateAsyncJobs_LiveTaskInDeletedTail pins doc
// sec.3.8's Rerun paragraph end to end: a bash job still RUNNING when its
// owning tool call is in the deleted tail is stopped, wake=0, and its row
// is voided -- neither a pullable notice nor a turn can follow.
func TestCoordinator_RerunTruncateAsyncJobs_LiveTaskInDeletedTail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	coord := newRerunTestCoordinator(t)

	_, _, err := coord.asyncJobs.Start("root", "call-bash", "", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	coord.asyncJobs.acknowledged("root", "call-bash")

	require.NoError(t, coord.RerunTruncateAsyncJobs(ctx, "root", []string{"call-bash"}, nil))

	row, err := coord.asyncJobs.store.Get(ctx, "root", "call-bash")
	require.NoError(t, err)
	require.NotEqual(t, "running", row.State, "the live task must be stopped")
	require.Equal(t, "void", row.Delivery, "a deleted-tail call's row must never surface as a notice")
	require.EqualValues(t, 0, row.Wake)

	pulled, err := coord.asyncJobs.store.PullJobNotices(ctx, nil, "root", buildJobNoticeMessageParams)
	require.NoError(t, err)
	require.Empty(t, pulled, "no notice may ever be pulled for a voided row")
}

// TestCoordinator_RerunTruncateAsyncJobs_DelegationTreeStoppedAndVoided
// extends the same scenario to a delegation: its child's own running job
// must be stopped too, recursively, with wake zeroed across the whole
// affected subtree (doc sec.3.4/3.8).
func TestCoordinator_RerunTruncateAsyncJobs_DelegationTreeStoppedAndVoided(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	coord := newRerunTestCoordinator(t)

	_, _, err := coord.asyncJobs.Start("root", "deleg-call", "", AgentToolName, "child-1", false, false, nil, func() {})
	require.NoError(t, err)
	coord.asyncJobs.acknowledged("root", "deleg-call")
	_, _, err = coord.asyncJobs.Start("child-1", "child-bash", "", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	coord.asyncJobs.acknowledged("child-1", "child-bash")

	require.NoError(t, coord.RerunTruncateAsyncJobs(ctx, "root", []string{"deleg-call"}, nil))

	delegRow, err := coord.asyncJobs.store.Get(ctx, "root", "deleg-call")
	require.NoError(t, err)
	require.Equal(t, "void", delegRow.Delivery)
	require.EqualValues(t, 0, delegRow.Wake)

	childRow, err := coord.asyncJobs.store.Get(ctx, "child-1", "child-bash")
	require.NoError(t, err)
	require.NotEqual(t, "running", childRow.State, "the child's own job must be stopped through the tree")
	require.EqualValues(t, 0, childRow.Wake, "Stop's wake-zero pass must cover the whole affected subtree")
}
