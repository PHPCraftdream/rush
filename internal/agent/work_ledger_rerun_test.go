// stopToolCallsForRerun coverage (doc sec.3.8, step 6): Rerun-scoped stop
// of RUNNING rows named by a deleted tail, recursively through a
// delegation's tree, exactly like Stop.
package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStopToolCallsForRerun_StopsPlainJobWithWakeZero pins the base case: a
// plain (bash/run_command) job named by the deleted tail is stopped in
// place, wake=0.
func TestStopToolCallsForRerun_StopsPlainJobWithWakeZero(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	store := newTestAsyncJobStore(t)
	l.store = store
	_, _, err := l.Start("owner-1", "call-1", "", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	l.acknowledged("owner-1", "call-1")

	affected := l.stopToolCallsForRerun("owner-1", []string{"call-1"})
	require.Empty(t, affected, "a plain job has no child session to fold into the affected set")

	row, err := store.Get(context.Background(), "owner-1", "call-1")
	require.NoError(t, err)
	require.NotEqual(t, "running", row.State)
	require.EqualValues(t, 0, row.Wake, "Rerun-stopped rows must never wake anyone (doc sec.3.8)")
}

// TestStopToolCallsForRerun_StopsDelegationTreeRecursively pins the
// delegation case: stopping a delegation's tool call ALSO stops its whole
// current tree (cancelTree), exactly like Stop -- a plain job the child
// itself owns must be stopped too, and the child session id must appear in
// the returned affected set.
func TestStopToolCallsForRerun_StopsDelegationTreeRecursively(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	store := newTestAsyncJobStore(t)
	l.store = store

	_, _, err := l.Start("owner-1", "deleg-call", "", AgentToolName, "child-1", false, false, nil, func() {})
	require.NoError(t, err)
	l.acknowledged("owner-1", "deleg-call")

	_, _, err = l.Start("child-1", "child-call", "", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	l.acknowledged("child-1", "child-call")

	affected := l.stopToolCallsForRerun("owner-1", []string{"deleg-call"})
	require.Contains(t, affected, "child-1", "the delegation's child session must be folded into the affected set")

	delegRow, err := store.Get(context.Background(), "owner-1", "deleg-call")
	require.NoError(t, err)
	require.NotEqual(t, "running", delegRow.State)
	require.EqualValues(t, 0, delegRow.Wake)

	childRow, err := store.Get(context.Background(), "child-1", "child-call")
	require.NoError(t, err)
	require.NotEqual(t, "running", childRow.State, "the child's own job must be stopped too, recursively")
	require.EqualValues(t, 0, childRow.Wake)
}

// TestStopToolCallsForRerun_UnknownOrTerminalToolCallIDIsANoOp pins the
// best-effort rule: a toolCallID with no RUNNING ledger entry (never
// started, or already terminal) is silently skipped, never an error.
func TestStopToolCallsForRerun_UnknownOrTerminalToolCallIDIsANoOp(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)

	require.NotPanics(t, func() {
		affected := l.stopToolCallsForRerun("owner-1", []string{"never-started"})
		require.Empty(t, affected)
	})
}
