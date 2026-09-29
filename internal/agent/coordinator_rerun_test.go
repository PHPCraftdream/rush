// Rerun's agent-layer contract (docs/reviews/2026-09-29-async-phase4-round1-
// rerun-design.md, Problem 1): CancelTurn cancels only the live generation
// (a rerun keeps history's running jobs, debt and autonomy), and
// StopRerunJobs stops exactly what a committed truncation voided -- the
// running executors of this process and every voided delegation's child
// tree. Real SQLite throughout.
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
	store    *session.AsyncJobStore
	sqlDB    *sql.DB
	messages message.Service
}

func newRerunCoordFx(t *testing.T) *rerunCoordFx {
	t.Helper()
	store, _, conn := newTestAsyncJobStoreWithDataDir(t)
	ag := &mockSessionAgent{}
	coord := &coordinator{currentAgent: ag, subAgentDrivers: newSubAgentDriverRegistry()}
	coord.asyncJobs = newWorkLedger(nil)
	coord.asyncJobs.store = store
	coord.asyncJobs.coord = coord
	return &rerunCoordFx{coord: coord, agent: ag, store: store, sqlDB: conn, messages: message.NewService(db.New(conn))}
}

// start registers an announced running job (the plain ack path).
func (f *rerunCoordFx) start(t *testing.T, owner, callID, tool, child string) {
	t.Helper()
	_, _, err := f.coord.asyncJobs.Start(owner, callID, "", tool, child, false, false, nil, func() {})
	require.NoError(t, err)
	f.coord.asyncJobs.acknowledged(owner, callID)
}

func (f *rerunCoordFx) row(t *testing.T, owner, callID string) db.AsyncJob {
	t.Helper()
	row, err := f.store.Get(context.Background(), owner, callID)
	require.NoError(t, err)
	return row
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
	f.start(t, "root", "call-run", "bash", "")
	f.start(t, "root", "call-debt", "bash", "")
	f.coord.asyncJobs.finish("root", "call-debt", jobResult{content: "out"})
	require.EqualValues(t, 1, f.row(t, "root", "call-debt").Wake, "precondition: unreacted debt")

	f.coord.CancelTurn("root")

	require.Equal(t, []string{"root"}, f.agent.cancelled, "the live generation must be cancelled")
	require.Equal(t, "running", f.row(t, "root", "call-run").State, "a kept running job must not be stopped by a rerun's CancelTurn")
	require.EqualValues(t, 1, f.row(t, "root", "call-debt").Wake, "CancelTurn must not zero wake")
	f.coord.autoResumeMu.Lock()
	suspended := f.coord.consecutiveAutoResumes["root"] >= maxConsecutiveAutoResumes
	f.coord.autoResumeMu.Unlock()
	require.False(t, suspended, "CancelTurn must not suspend autonomy")
}

// TestCancelTurn_NilAgentIsANoOp: a coordinator with no agent for the id does
// not panic.
func TestCancelTurn_NilAgentIsANoOp(t *testing.T) {
	t.Parallel()
	require.NotPanics(t, func() { (&coordinator{}).CancelTurn("root") })
}

// TestStopRerunJobs_StopsRunningAndChildTree: the running executor of a
// voided row is stopped (job_kill semantics), and a voided delegation's
// child tree is stopped like Stop -- its own running job, its mailbox, wake
// zeroed, autonomy suspended for the child.
//
// Revert-check: omit stopTree(child) in StopRerunJobs.
func TestStopRerunJobs_StopsRunningAndChildTree(t *testing.T) {
	t.Parallel()
	f := newRerunCoordFx(t)
	f.start(t, "root", "call-bash", "bash", "")
	f.start(t, "root", "deleg-call", AgentToolName, "child-1")
	f.start(t, "child-1", "child-bash", "bash", "")

	f.coord.StopRerunJobs(context.Background(), "root", []session.VoidedAsyncJob{
		{ToolCallID: "call-bash", State: "running", HostID: f.store.HostID()},
		{ToolCallID: "deleg-call", State: "running", ChildSessionID: "child-1", HostID: f.store.HostID()},
		{ToolCallID: "foreign-call", State: "running", HostID: "another-host"}, // no local executor: skipped
	})

	require.NotEqual(t, "running", f.row(t, "root", "call-bash").State, "the voided plain job must be stopped")
	require.NotEqual(t, "running", f.row(t, "root", "deleg-call").State, "the voided delegation must be stopped")
	child := f.row(t, "child-1", "child-bash")
	require.NotEqual(t, "running", child.State, "the delegation's child tree must be stopped")
	require.EqualValues(t, 0, child.Wake)
	require.Contains(t, f.agent.cancelled, "child-1", "the child's live generation must be cancelled")
	f.coord.autoResumeMu.Lock()
	suspended := f.coord.consecutiveAutoResumes["child-1"] >= maxConsecutiveAutoResumes
	f.coord.autoResumeMu.Unlock()
	require.True(t, suspended, "the stopped child must not resume autonomously")
}

// TestStopRerunJobs_TerminalVoidedDelegationStillStopsChildTree: a voided
// delegation whose own row already finished still has its child tree
// stopped (its child's work belongs to the deleted call).
func TestStopRerunJobs_TerminalVoidedDelegationStillStopsChildTree(t *testing.T) {
	t.Parallel()
	f := newRerunCoordFx(t)
	f.start(t, "child-1", "child-bash", "bash", "")

	f.coord.StopRerunJobs(context.Background(), "root", []session.VoidedAsyncJob{
		{ToolCallID: "deleg-call", State: "completed", ChildSessionID: "child-1", HostID: f.store.HostID()},
	})

	require.NotEqual(t, "running", f.row(t, "child-1", "child-bash").State)
}

// TestStopRerunJobs_NoStoreOrEmptyIsANoOp: nothing voided, nothing to do.
func TestStopRerunJobs_NoStoreOrEmptyIsANoOp(t *testing.T) {
	t.Parallel()
	require.NotPanics(t, func() {
		(&coordinator{}).StopRerunJobs(context.Background(), "root", []session.VoidedAsyncJob{{ToolCallID: "x", State: "running"}})
		newRerunCoordFx(t).coord.StopRerunJobs(context.Background(), "root", nil)
	})
}

// TestRerunKeptRunningJob_ResultReachesNewBranch: a job of the KEPT history
// still running through a rerun completes afterwards and its result is
// still delivered -- pending, wake=1, pullable by the replacement turn.
//
// Revert-check: use the full Stop (Cancel) instead of CancelTurn for the
// rerun's cancel -- the kept job is stopped and its notice never arrives.
func TestRerunKeptRunningJob_ResultReachesNewBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRerunCoordFx(t)
	f.start(t, "root", "call-kept", "bash", "")

	f.coord.CancelTurn("root") // the rerun's cancel
	f.coord.asyncJobs.finish("root", "call-kept", jobResult{content: "kept result"})

	row := f.row(t, "root", "call-kept")
	require.Equal(t, "completed", row.State, "the kept job must run to its natural end")
	require.Equal(t, "pending", row.Delivery)
	require.EqualValues(t, 1, row.Wake)
	pulled, err := f.store.PullJobNotices(ctx, f.messages, "root", buildJobNoticeMessageParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1, "the replacement turn must be able to pull the kept job's notice")
}

// TestRerun_TruncateThenStop_TailJobVoidedAndStoppedKeptJobSurvives: the
// whole Problem-1 contract across both layers: history keeps job K (before
// the target) and has job T in the tail. TruncateForRerun voids only T;
// StopRerunJobs stops only T; K keeps running, undisturbed.
func TestRerun_TruncateThenStop_TailJobVoidedAndStoppedKeptJobSurvives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRerunCoordFx(t)

	add := func(role message.MessageRole, parts ...message.ContentPart) message.Message {
		m, err := f.messages.Create(ctx, "root", message.CreateMessageParams{Role: role, Parts: parts})
		require.NoError(t, err)
		return m
	}
	announce := func(callID string) message.Message {
		_, _, err := f.coord.asyncJobs.Start("root", callID, "", "bash", "", false, false, nil, func() {})
		require.NoError(t, err)
		msg, handled, err := f.coord.asyncJobs.acknowledgeWithMessageTx(ctx, "root", callID, f.messages, message.CreateMessageParams{
			Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: callID, Name: "bash", Content: "started"}},
		})
		require.NoError(t, err)
		require.True(t, handled)
		return msg
	}
	call := func(id string) message.Message {
		return add(message.Assistant, message.ToolCall{ID: id, Name: "bash", Input: "{}", Finished: true})
	}

	call("call_0")
	announce("call_0") // kept job K
	target := add(message.User, message.TextContent{Text: "rerun me"})
	tailCall := call("call_1")
	tailStarted := announce("call_1") // tail job T

	res, err := session.TruncateForRerun(ctx, f.sqlDB, f.messages, session.RerunTruncateParams{
		Owner: "root", TargetID: target.ID, TailIDs: []string{tailCall.ID, tailStarted.ID},
	})
	require.NoError(t, err)
	require.Len(t, res.Voided, 1)
	require.Equal(t, "call_1", res.Voided[0].ToolCallID)

	f.coord.StopRerunJobs(ctx, "root", res.Voided)

	tail := f.row(t, "root", "call_1")
	require.NotEqual(t, "running", tail.State, "the tail's job must be stopped")
	require.Equal(t, "void", tail.Delivery)
	kept := f.row(t, "root", "call_0")
	require.Equal(t, "running", kept.State, "the kept job must keep running")
	require.Equal(t, "none", kept.Delivery)
}
