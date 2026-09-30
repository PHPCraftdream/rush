// stopToolCallsForRerun coverage (doc sec.3.8, step 6): Rerun-scoped stop
// of the RUNNING rows a committed truncation voided. It stops exactly the
// named jobs; the delegation child tree is StopRerunJobs's stopTree (see
// coordinator_rerun_test.go).
package agent

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
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
	l.acknowledged(jobOf(l, "owner-1", "call-1"))

	l.stopToolCallsForRerun("owner-1", []string{"call-1"})

	row, err := store.Get(context.Background(), "owner-1", "call-1")
	require.NoError(t, err)
	require.NotEqual(t, "running", row.State)
	require.EqualValues(t, 0, row.Wake, "Rerun-stopped rows must never wake anyone (doc sec.3.8)")
}

// TestStopToolCallsForRerun_DelegationStoppedChildTreeIsCallersJob pins the
// split: the delegation's own job is stopped, but its child's jobs are NOT
// touched here -- Coordinator.StopRerunJobs walks the tree with stopTree,
// which also cancels the child's generation (cancelTree alone did neither).
func TestStopToolCallsForRerun_DelegationStoppedChildTreeIsCallersJob(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	store := newTestAsyncJobStore(t)
	l.store = store

	_, _, err := l.Start("owner-1", "deleg-call", "", AgentToolName, "child-1", false, false, nil, func() {})
	require.NoError(t, err)
	l.acknowledged(jobOf(l, "owner-1", "deleg-call"))
	_, _, err = l.Start("child-1", "child-call", "", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	l.acknowledged(jobOf(l, "child-1", "child-call"))

	l.stopToolCallsForRerun("owner-1", []string{"deleg-call"})

	delegRow, err := store.Get(context.Background(), "owner-1", "deleg-call")
	require.NoError(t, err)
	require.NotEqual(t, "running", delegRow.State)
	require.EqualValues(t, 0, delegRow.Wake)

	childRow, err := store.Get(context.Background(), "child-1", "child-call")
	require.NoError(t, err)
	require.Equal(t, "running", childRow.State, "the child tree is stopped by StopRerunJobs, not by the ledger-level stop")
}

// TestStopToolCallsForRerun_UnknownOrTerminalToolCallIDIsANoOp pins the
// best-effort rule: a toolCallID with no RUNNING ledger entry (never
// started, or already terminal) is silently skipped, never an error.
func TestStopToolCallsForRerun_UnknownOrTerminalToolCallIDIsANoOp(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	l.store = newTestAsyncJobStore(t)

	require.NotPanics(t, func() {
		l.stopToolCallsForRerun("owner-1", []string{"never-started"})
	})
}

// TestStopToolCallsForRerun_RecordsStopSemanticsAndKeepsVoid pins what
// `sessions jobs` shows for a Rerun-stopped row (R2B-20): Stop semantics --
// state cancelled, notice_kind session_cancel, no wake -- and the void that
// Rerun already set survives the terminal transition. (The stop is NOT
// job_kill's: that would be notice_kind job_kill, delivery done.)
//
// Revert-check: stopping with causeJobKill instead of causeSessionCancel
// changed notice_kind to job_kill and failed the assertion.
func TestStopToolCallsForRerun_RecordsStopSemanticsAndKeepsVoid(t *testing.T) {
	t.Parallel()
	l := newWorkLedger(nil)
	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
	l.store = store
	_, _, err := l.Start("owner-1", "call-1", "", "bash", "", false, false, nil, func() {})
	require.NoError(t, err)
	l.acknowledged(jobOf(l, "owner-1", "call-1"))
	// What TruncateForRerun does to a running row of the deleted tail.
	rows, err := db.New(conn).VoidAsyncJobsByToolCallIDs(context.Background(), db.VoidAsyncJobsByToolCallIDsParams{
		UpdatedAt: time.Now().Unix(), OwnerSessionID: "owner-1", ToolCallIds: []string{"call-1"},
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)

	l.stopToolCallsForRerun("owner-1", []string{"call-1"})

	row, err := store.Get(context.Background(), "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "cancelled", row.State)
	require.Equal(t, "session_cancel", row.NoticeKind)
	require.EqualValues(t, 0, row.Wake)
	require.Equal(t, "void", row.Delivery, "a terminal transition preserves the void Rerun set")
}
