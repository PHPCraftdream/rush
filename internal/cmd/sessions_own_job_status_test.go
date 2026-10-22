package cmd

// A root whose scope is open only because of its OWN running batch work
// (no delegation row, no lock between turns) is working: `sessions why`
// reports it batch live through the batch facts, and `sessions list` keeps
// it non-terminal through the one classifier. A dead recorded PID plus live
// own work reads batch live with the stale-lock annotation; once the work is
// terminal the dead-PID lock is a crash (D3/D5).

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// batchSeedHost is the shared seed substrate: it registers the host identity
// the batch reservation needs with this live test process (session.HostStore
// over the app's DB), so the live reader proves liveness via the own-host
// registry. Returns the host and a BatchStore over the app's DB.
func batchSeedHost(t *testing.T, a *app.App) (string, *session.BatchStore) {
	t.Helper()
	store := session.NewHostStore(a.DB(), a.DataDirectory(), os.Getpid(), "batch-seed-fixture")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	host, err := store.EnsureHost(context.Background())
	require.NoError(t, err)
	return host, session.NewBatchStore(a.DB())
}

// settleBatchRoot is the finishOwnJob-style raw UPDATE: it settles the root
// reserved for (owner, toolCallID) and all of its nodes to terminal (raw
// UPDATE on rows Reserve created -- the established pattern for
// notice/debt columns; Project needs a live host identity the detached seed
// no longer has).
func settleBatchRoot(t *testing.T, a *app.App, owner, toolCallID string) {
	t.Helper()
	_, err := a.DB().ExecContext(context.Background(),
		`UPDATE batch_nodes SET state = 'completed', control_state = 'none', updated_at = ?
		 WHERE owner_session_id = ? AND (tool_call_id = ? OR root_id IN
			 (SELECT root_id FROM batch_nodes WHERE owner_session_id = ? AND tool_call_id = ?))`,
		time.Now().Unix(), owner, toolCallID, owner, toolCallID)
	require.NoError(t, err)
}

// claimOwnJob seeds the "own running batch work" fact for owner: a live
// batch root (kind 'batch' with one 'command' task child -- 'command' keeps
// it a command, not an agent delegation).
func claimOwnJob(t *testing.T, a *app.App, owner, toolCallID string) {
	t.Helper()
	host, bs := batchSeedHost(t, a)
	_, err := bs.Reserve(context.Background(), session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "claim-own-job-runner",
		ToolCallID: toolCallID, InputHash: "hash-" + toolCallID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "pending", ControlState: "none",
		}, Children: []session.BatchReservationNode{
			{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "pending", ControlState: "none"}},
		}},
	})
	require.NoError(t, err)
}

// Revert-check: any re-introduction of a local own-job promotion (or its
// omission from the batch facts) flips the first line back to
// "status: at rest" and fails the first assertion.
func TestExplainSessionStatus_AtRestOwnRunningJobIsBatchLive(t *testing.T) {
	a, s, m, _, dataDir := newWhyDescendantTestApp(t)
	sess, err := s.Create(context.Background(), "root waiting on its own bash job")
	require.NoError(t, err)
	addFinishedAssistant(t, m, sess.ID, message.FinishReasonEndTurn)
	claimOwnJob(t, a, sess.ID, "bash-1")

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &buf))
	out := buf.String()

	require.Equal(t, "status: batch live", strings.SplitN(out, "\n", 2)[0],
		"own running batch work keeps the session non-terminal")
	require.Contains(t, out, "reason: batch root ", "the reason names the held root")
	require.Contains(t, out, "bash-1", "the reason names the held work's tool call")
	require.NotContains(t, out, "status: at rest")

	settleBatchRoot(t, a, sess.ID, "bash-1")
	buf.Reset()
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &buf))
	require.Equal(t, "status: at rest", strings.SplitN(buf.String(), "\n", 2)[0],
		"a finished job no longer keeps the session non-terminal")
}

func TestExplainSessionStatus_DeadPIDLockWithOwnJobIsBatchLiveNotCrashed(t *testing.T) {
	// D1: live work outranks the dead recorded PID, which becomes the
	// "(stale lock)" annotation on the batch-live verdict.
	a, s, m, _, dataDir := newWhyDescendantTestApp(t)
	sess, err := s.Create(context.Background(), "stale-lock root waiting on its own job")
	require.NoError(t, err)
	backDateLock(t, writeLockFileAt(t, dataDir, sess.ID, 999999))
	addFinishedAssistant(t, m, sess.ID, message.FinishReasonEndTurn)
	claimOwnJob(t, a, sess.ID, "bash-1")

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &buf))
	out := buf.String()

	require.Equal(t, "status: batch live (stale lock)", strings.SplitN(out, "\n", 2)[0])
	require.Contains(t, out, "dead PID 999999")
	require.NotContains(t, out, "status: done")
}

func TestExplainSessionStatus_DeadPIDLockWithoutJobIsCrashed(t *testing.T) {
	// D3: with the work terminal there is no live batch work; the recorded
	// dead PID (the holder never reached release) is a crash even with an
	// end_turn-style finish from a previous turn.
	a, s, m, _, dataDir := newWhyDescendantTestApp(t)
	sess, err := s.Create(context.Background(), "crashed root, work finished")
	require.NoError(t, err)
	backDateLock(t, writeLockFileAt(t, dataDir, sess.ID, 999999))
	addFinishedAssistant(t, m, sess.ID, message.FinishReasonError)
	claimOwnJob(t, a, sess.ID, "bash-1")
	settleBatchRoot(t, a, sess.ID, "bash-1")

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &buf))
	require.Equal(t, "status: crashed", strings.SplitN(buf.String(), "\n", 2)[0],
		"a dead holder with nothing live is the session's own crash, whatever the last finish")
}

// TestListStatus_OwnJobShapes pins the classifier STATUS vocabulary across
// the batch-fact shapes (the deleted per-command layer's replacement).
// Revert-check: any local re-ranking of these shapes in a reader breaks its
// row here while the other rows still pass.
func TestListStatus_OwnJobShapes(t *testing.T) {
	a, s, _, _, dataDir := newWhyDescendantTestApp(t)
	ctx := context.Background()
	mk := func(title string) session.Session {
		sess, err := s.Create(ctx, title)
		require.NoError(t, err)
		return sess
	}
	blank, deadPID, withJob := mk("blank"), mk("dead-pid"), mk("both")

	claimOwnJob(t, a, blank.ID, "bash-1")
	claimOwnJob(t, a, deadPID.ID, "bash-1")
	backDateLock(t, writeLockFileAt(t, dataDir, deadPID.ID, 999999))

	claimOwnJob(t, a, withJob.ID, "bash-1")
	other := mk("other")
	claimDelegation(t, a, withJob.ID, "agent-1", other.ID)

	acts, err := a.SessionActivityBatch(ctx, []string{blank.ID, deadPID.ID, withJob.ID})
	require.NoError(t, err)
	for _, act := range acts.ByID {
		require.Empty(t, act.Facts.Unreadable)
	}
	require.Equal(t, "running", listStatus(acts.ByID[blank.ID].Verdict),
		"own running batch work is classifier running")
	require.Equal(t, "running", listStatus(acts.ByID[deadPID.ID].Verdict), "D1: the live work outranks the dead PID")
	require.Equal(t, "delegating", listStatus(acts.ByID[withJob.ID].Verdict), "D2: the delegation is the Kind, the job a wait")
	require.Contains(t, acts.ByID[withJob.ID].Verdict.WaitingOn, session.WaitTasks)
}

// TestSessionsListCmdRun_OwnRunningJobIsRunningNotDone drives the real
// `sessions list`: a parent with a dead-PID lock and a clean end_turn that
// owns running batch work must read "running", and read "crashed" once the
// work is terminal (D3: a dead PID with nothing live is a crash -- the old
// "done" reclassification by finish reason is gone, D4).
func TestSessionsListCmdRun_OwnRunningJobIsRunningNotDone(t *testing.T) {
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()

	parent, err := a.Sessions.CreateWithID(ctx, "list-own-job-root", "root waiting on its own job")
	require.NoError(t, err)

	lockDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	parentLock := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(parent.ID)+".lock")
	require.NoError(t, os.WriteFile(parentLock, []byte("999999\n"), 0o644))
	backDateLock(t, parentLock)

	assistant, err := a.Messages.Create(ctx, parent.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "started a background job"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, assistant))

	host, _ := batchSeedHost(t, a)
	seedCmdBatchRootFor(t, a, parent.ID, host, "bash-1")
	// The seed HostStore stays open until cleanup, so its OS lock stays held
	// across the Shutdown and the command's host probe reads the host alive
	// (no second acquire: holdSeedHost would contend with the held lock).
	a.Shutdown()

	runList := func() string {
		return captureStdout(t, func() {
			require.NoError(t, sessionsListCmd.RunE(sessionsListCmd, nil))
		})
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
	require.Contains(t, phase1, "running", "a root waiting on its own running work is working, not done (D1)")
	require.NotContains(t, phase1, "done")

	freshConn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	// Settle the batch root with the raw UPDATE (the seed app is shut down;
	// write through a fresh conn).
	_, err = freshConn.ExecContext(ctx,
		`UPDATE batch_nodes SET state = 'completed', control_state = 'none', updated_at = ?
		 WHERE owner_session_id = ? AND (tool_call_id = ? OR root_id IN
			 (SELECT root_id FROM batch_nodes WHERE owner_session_id = ? AND tool_call_id = ?))`,
		time.Now().Unix(), parent.ID, "bash-1", parent.ID, "bash-1")
	require.NoError(t, err)

	phase2 := rowFor(runList())
	// D3/D4: with the work terminal there is no live work; the recorded dead
	// PID is a crash, and the end_turn finish is not an end signal.
	require.Contains(t, phase2, "crashed")
	require.NotContains(t, phase2, "running")
}
