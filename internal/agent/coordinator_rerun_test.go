// Rerun's agent-layer contract (docs/reviews/2026-09-29-async-phase4-round1-
// rerun-design.md, Problem 1), batch edition: CancelTurn cancels only the
// live generation (a rerun keeps the live batch roots, debt and autonomy).
// The deleted StopRerunJobs/ledger-stop tests are obsolete contracts: the
// committed-truncation stop is BatchRerunStopVoided over batch nodes, pinned
// end-to-end over the real engine by batch_cutover_rerun_test.go.
package agent

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type rerunCoordFx struct {
	coord    *coordinator
	agent    *mockSessionAgent
	conn     *sql.DB
	messages message.Service
}

func newRerunCoordFx(t *testing.T) *rerunCoordFx {
	t.Helper()
	conn, _ := batchFixtureConn(t)
	ag := &mockSessionAgent{}
	coord := newBatchRuntimeCoordinator()
	coord.currentAgent = ag
	bindBatchNodeFacts(t, coord, conn, "rerun-fixture-dir")
	return &rerunCoordFx{coord: coord, agent: ag, conn: conn, messages: message.NewService(db.New(conn))}
}

// start seeds a LIVE batch root for (owner, callID) — the running work a
// rerun's CancelTurn must not disturb.
func (f *rerunCoordFx) start(t *testing.T, owner, callID, tool, child string) {
	t.Helper()
	seedBatchDelegation(t, f.conn, owner, callID, child, tool, "running", false)
}

// nodeState reads the ROOT node's state for (owner, callID).
func (f *rerunCoordFx) rootState(t *testing.T, owner, callID string) (string, int64) {
	t.Helper()
	var state string
	var wake int64
	require.NoError(t, f.conn.QueryRowContext(context.Background(),
		`SELECT state, wake FROM batch_nodes WHERE owner_session_id=? AND tool_call_id=? AND node_id=root_id ORDER BY rowid DESC LIMIT 1`,
		owner, callID).Scan(&state, &wake))
	return state, wake
}

// TestCancelTurn_LeavesJobsWakeAndAutonomy: unlike Cancel (the full Stop),
// CancelTurn reaches only the session's own agent: a running job keeps
// running, an unreacted notice keeps its wake bit, autonomy is not
// suspended.
//
// Revert-check: make CancelTurn call stopTree (== Cancel).
func TestCancelTurn_LeavesJobsWakeAndAutonomy(t *testing.T) {
	t.Parallel()
	f := newRerunCoordFx(t)
	f.start(t, "root", "call-run", "command", "")
	f.start(t, "root", "call-debt", "command", "")
	seedBatchJobDebt(t, f.conn, "root", "call-debt-done", false)
	_, debtWake := f.rootState(t, "root", "call-debt-done")
	require.EqualValues(t, 1, debtWake, "precondition: unreacted wake debt")

	f.coord.CancelTurn("root")

	require.Equal(t, []string{"root"}, f.agent.cancelled, "the live generation must be cancelled")
	runState, _ := f.rootState(t, "root", "call-run")
	require.Equal(t, "running", runState, "a kept running root must not be stopped by a rerun's CancelTurn")
	_, debtWakeAfter := f.rootState(t, "root", "call-debt-done")
	require.EqualValues(t, 1, debtWakeAfter, "CancelTurn must not zero wake")
	require.False(t, f.coord.autoResumeSuspended("root"), "CancelTurn must not suspend autonomy")
}

// TestCancelTurn_NilAgentIsANoOp: a coordinator with no agent for the id does
// not panic.
func TestCancelTurn_NilAgentIsANoOp(t *testing.T) {
	t.Parallel()
	require.NotPanics(t, func() { (&coordinator{}).CancelTurn("root") })
}

// TestStopRerunJobs_StopsRunningAndChildTree,
// TestStopRerunJobs_TerminalVoidedDelegationStillStopsChildTree,
// TestRerunKeptRunningJob_ResultReachesNewBranch and
// TestRerun_TruncateThenStop_TailJobVoidedAndStoppedKeptJobSurvives were
// DELETEd as obsolete contracts: they pinned the deleted StopRerunJobs over
// session.VoidedAsyncJob and the ledger's acknowledgeWithMessageTx/pull
// plumbing. The batch contract (BatchRerunStopVoided stops exactly the
// voided incarnations' engine roots and every voided delegation's child
// tree, while kept history's live roots run on) is pinned end-to-end over
// the real jobbatch engine by batch_cutover_rerun_test.go.

// TestBatchRerunStopVoided_NoBindingOrEmptyIsANoOp: nothing voided, or no
// runtime bound -- nothing to do, no panic.
func TestBatchRerunStopVoided_NoBindingOrEmptyIsANoOp(t *testing.T) {
	t.Parallel()
	require.NotPanics(t, func() {
		(&coordinator{}).BatchRerunStopVoided(context.Background(), "root", []session.BatchRerunVoidedNode{{ToolCallID: "x", State: "running"}})
		newRerunCoordFx(t).coord.BatchRerunStopVoided(context.Background(), "root", nil)
	})
}
