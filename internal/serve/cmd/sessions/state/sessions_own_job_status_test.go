package sessstate

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/sesskit"
	"github.com/PHPCraftdream/rush/internal/session"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	sessionfacts "github.com/PHPCraftdream/rush/internal/session/state/sessionfacts"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

func stateClaimOwnJob(t *testing.T, a *app.App, owner, toolCallID string) {
	t.Helper()
	host, store := cmdtest.BatchSeedHost(t, a)
	_, err := store.Reserve(context.Background(), session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "claim-own-job-runner",
		ToolCallID: toolCallID, InputHash: "hash-" + toolCallID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "pending", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "pending", ControlState: "none",
		}}}},
	})
	require.NoError(t, err)
}

func stateSettleOwnJob(t *testing.T, a *app.App, owner, toolCallID string) {
	t.Helper()
	_, err := a.DB().ExecContext(context.Background(),
		`UPDATE batch_nodes SET state = 'completed', control_state = 'none', updated_at = ?
		 WHERE owner_session_id = ? AND (tool_call_id = ? OR root_id IN
			 (SELECT root_id FROM batch_nodes WHERE owner_session_id = ? AND tool_call_id = ?))`,
		time.Now().Unix(), owner, toolCallID, owner, toolCallID)
	require.NoError(t, err)
}

// TestExplainSessionStatus_AtRestOwnRunningJobIsBatchLive exercises the state
// explanation directly: own running batch work keeps an otherwise idle root live.
func TestExplainSessionStatus_AtRestOwnRunningJobIsBatchLive(t *testing.T) {
	a, s, m, _, dataDir := cmdtest.NewWhyDescendantTestApp(t)
	sess, err := s.Create(context.Background(), "root waiting on its own bash job")
	require.NoError(t, err)
	cmdtest.AddFinishedAssistant(t, m, sess.ID, message.FinishReasonEndTurn)
	stateClaimOwnJob(t, a, sess.ID, "bash-1")

	var out bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &out))
	require.Equal(t, "status: batch live", strings.SplitN(out.String(), "\n", 2)[0],
		"own running batch work keeps the session non-terminal")
	require.Contains(t, out.String(), "reason: batch root ", "the reason names the held root")
	require.Contains(t, out.String(), "bash-1", "the reason names the held work's tool call")
	require.NotContains(t, out.String(), "status: at rest")

	stateSettleOwnJob(t, a, sess.ID, "bash-1")
	out.Reset()
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &out))
	require.Equal(t, "status: at rest", strings.SplitN(out.String(), "\n", 2)[0])
}

func TestExplainSessionStatus_DeadPIDLockWithOwnJobIsBatchLiveNotCrashed(t *testing.T) {
	a, s, m, _, dataDir := cmdtest.NewWhyDescendantTestApp(t)
	sess, err := s.Create(context.Background(), "stale-lock root waiting on its own job")
	require.NoError(t, err)
	cmdtest.BackDateLock(t, cmdtest.WriteLockFileAt(t, dataDir, sess.ID, 999999))
	cmdtest.AddFinishedAssistant(t, m, sess.ID, message.FinishReasonEndTurn)
	stateClaimOwnJob(t, a, sess.ID, "bash-1")

	var out bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &out))
	require.Equal(t, "status: batch live (stale lock)", strings.SplitN(out.String(), "\n", 2)[0])
	require.Contains(t, out.String(), "dead PID 999999")
	require.NotContains(t, out.String(), "status: done")
}

func TestExplainSessionStatus_DeadPIDLockWithoutJobIsCrashed(t *testing.T) {
	a, s, m, _, dataDir := cmdtest.NewWhyDescendantTestApp(t)
	sess, err := s.Create(context.Background(), "crashed root, work finished")
	require.NoError(t, err)
	cmdtest.BackDateLock(t, cmdtest.WriteLockFileAt(t, dataDir, sess.ID, 999999))
	cmdtest.AddFinishedAssistant(t, m, sess.ID, message.FinishReasonError)
	stateClaimOwnJob(t, a, sess.ID, "bash-1")
	stateSettleOwnJob(t, a, sess.ID, "bash-1")

	var out bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &out))
	require.Equal(t, "status: crashed", strings.SplitN(out.String(), "\n", 2)[0])
}

// TestListStatus_OwnJobShapes covers the batched classifier path independently
// from the real scan List command exercised below.
func TestListStatus_OwnJobShapes(t *testing.T) {
	a, s, _, _, dataDir := cmdtest.NewWhyDescendantTestApp(t)
	ctx := context.Background()
	mk := func(title string) coresession.Session {
		sess, err := s.Create(ctx, title)
		require.NoError(t, err)
		return sess
	}
	blank, deadPID, withJob := mk("blank"), mk("dead-pid"), mk("both")
	stateClaimOwnJob(t, a, blank.ID, "bash-1")
	stateClaimOwnJob(t, a, deadPID.ID, "bash-1")
	cmdtest.BackDateLock(t, cmdtest.WriteLockFileAt(t, dataDir, deadPID.ID, 999999))
	stateClaimOwnJob(t, a, withJob.ID, "bash-1")
	other := mk("other")
	cmdtest.ClaimDelegation(t, a, withJob.ID, "agent-1", other.ID)

	ids := []string{blank.ID, deadPID.ID, withJob.ID}
	activities, err := a.SessionActivityBatch(ctx, ids)
	require.NoError(t, err)
	statuses, _ := sesskit.ListStatusesWithAwaiting(ctx, sesskit.SessionQuestionReaderForApp(a), activities, ids)
	for _, act := range activities.ByID {
		require.Empty(t, act.Facts.Unreadable)
	}
	require.Equal(t, "running", statuses[blank.ID], "own running batch work is classifier running")
	require.Equal(t, "running", statuses[deadPID.ID], "live work outranks the dead PID")
	require.Equal(t, "delegating", statuses[withJob.ID], "delegation is the kind, the job a wait")
	require.Contains(t, activities.ByID[withJob.ID].Verdict.WaitingOn, sessionfacts.WaitTasks)
}

// TestSessionsListCmdRun_OwnRunningJobIsRunningNotDone exercises the actual
// scan List command path (separate from the classifier assertions above).
func TestSessionsListCmdRun_OwnRunningJobIsRunningNotDone(t *testing.T) {
	a, listCmd, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()
	parent, err := a.Sessions.CreateWithID(ctx, "list-own-job-root", "root waiting on its own job")
	require.NoError(t, err)
	lockPath := cmdtest.WriteLockFileAt(t, dataDir, parent.ID, 999999)
	cmdtest.BackDateLock(t, lockPath)
	cmdtest.AddFinishedAssistant(t, a.Messages, parent.ID, message.FinishReasonEndTurn)
	host, _ := cmdtest.BatchSeedHost(t, a)
	cmdtest.SeedCmdBatchRootFor(t, a, parent.ID, host, "bash-1")
	a.Shutdown()

	runList := func() string {
		return cmdtest.CaptureStdout(t, func() { require.NoError(t, listCmd.RunE(listCmd, nil)) })
	}
	rowFor := func(stdout string) string {
		for _, line := range strings.Split(stdout, "\n") {
			if strings.Contains(line, parent.ID[:8]) {
				return line
			}
		}
		return ""
	}
	phase1 := rowFor(runList())
	require.NotEmpty(t, phase1)
	require.Contains(t, phase1, "running")
	require.NotContains(t, phase1, "done")

	freshConn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	_, err = freshConn.ExecContext(ctx,
		`UPDATE batch_nodes SET state = 'completed', control_state = 'none', updated_at = ?
		 WHERE owner_session_id = ? AND (tool_call_id = ? OR root_id IN
			 (SELECT root_id FROM batch_nodes WHERE owner_session_id = ? AND tool_call_id = ?))`,
		time.Now().Unix(), parent.ID, "bash-1", parent.ID, "bash-1")
	require.NoError(t, err)
	phase2 := rowFor(runList())
	require.Contains(t, phase2, "crashed")
	require.NotContains(t, phase2, "running")
}
