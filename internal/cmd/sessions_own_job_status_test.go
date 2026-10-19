package cmd

// A root whose scope is open only because of its OWN running plain
// background job (bash/run_command; no delegation row, no lock between
// turns) is working: both `sessions why` and `sessions list` report it
// through the one classifier ("running", wait: tasks). Under the rework's
// decisions the never-downgrade rule INVERTED for this shape: live work
// outranks a dead recorded PID (D1), so a dead-PID lock plus a live own job
// is "running (stale lock)", not crashed; and a running row on a DEAD host
// with nothing live is "crashed" (D5), not done.

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
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func mustDBConn(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	return conn
}

// batchSeedHost is the shared dual-mode seed substrate: it resolves the
// host identity the batch reservation needs (the app's own registered host,
// so the live reader proves liveness via the own-host registry), tolerating
// a nil a.AsyncJobStore() (setupAppLite fixtures) with the same fallback
// dance seedBatchReadRoot uses. Returns the host, a BatchStore over the
// app's DB, and the fallback closer (nil when the app's own store was used).
func batchSeedHost(t *testing.T, a *app.App) (string, *session.BatchStore, func()) {
	t.Helper()
	ctx := context.Background()
	store := a.AsyncJobStore()
	var closeAsync func()
	if store == nil {
		store = session.NewAsyncJobStore(a.DB(), a.Config().Options.DataDirectory, os.Getpid(), "batch-seed-fixture")
		closeAsync = func() { _ = store.Close(context.Background()) }
		t.Cleanup(closeAsync)
	}
	host, err := store.EnsureHost(ctx)
	require.NoError(t, err)
	return host, session.NewBatchStore(a.DB()), closeAsync
}

// settleBatchRoot is the finishOwnJob-style raw UPDATE for the flag-ON
// substrate: it settles the root reserved for (owner, toolCallID) and all of
// its nodes to terminal (raw UPDATE on rows Reserve created -- the
// established pattern for notice/debt columns; Project needs a live host
// identity the detached seed no longer has).
func settleBatchRoot(t *testing.T, a *app.App, owner, toolCallID string) {
	t.Helper()
	_, err := a.DB().ExecContext(context.Background(),
		`UPDATE batch_nodes SET state = 'completed', control_state = 'none', updated_at = ?
		 WHERE owner_session_id = ? AND (tool_call_id = ? OR root_id IN
			 (SELECT root_id FROM batch_nodes WHERE owner_session_id = ? AND tool_call_id = ?))`,
		time.Now().Unix(), owner, toolCallID, owner, toolCallID)
	require.NoError(t, err)
}

// claimOwnJob seeds the "own running plain background job" fact for owner:
// flag off, the legacy async_jobs claim; flag on, the same fact as a live
// batch root (kind 'batch' with one 'command' task child -- 'command' keeps
// it a command, not an Agents delegation).
func claimOwnJob(t *testing.T, a *app.App, owner, toolCallID string) {
	t.Helper()
	if !config.BatchRuntimeEnabled() {
		store := a.AsyncJobStore()
		require.NotNil(t, store)
		_, err := store.Claim(context.Background(), session.ClaimParams{
			Owner: owner, ToolCallID: toolCallID, Kind: session.JobKindCommand, Input: "sleep 60", ToolName: "bash",
		})
		require.NoError(t, err)
		return
	}
	host, bs, _ := batchSeedHost(t, a)
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

// finishOwnJob settles the own job for (owner, toolCallID): flag off, the
// legacy Transition; flag on, the raw root settle (settleBatchRoot).
func finishOwnJob(t *testing.T, a *app.App, owner, toolCallID string) {
	t.Helper()
	if !config.BatchRuntimeEnabled() {
		store := a.AsyncJobStore()
		require.NotNil(t, store)
		_, err := store.Transition(context.Background(), session.TransitionParams{
			Owner: owner, ToolCallID: toolCallID, State: "completed", NoticeKind: "completed", Wake: true,
		})
		require.NoError(t, err)
		return
	}
	settleBatchRoot(t, a, owner, toolCallID)
}

// Revert-check: any re-introduction of a local own-job promotion (or its
// omission from the classifier) flips the first line back to
// "status: at rest" and fails the first assertion.
func TestExplainSessionStatus_AtRestOwnRunningJobIsRunning(t *testing.T) {
	// F11 env pin: pins the LEGACY why-render first line ("status: running");
	// under the flag explainSessionStatus routes to explainWhyBatch.
	t.Setenv("RUSH_BATCH_RUNTIME", "false")
	a, s, m, _, dataDir := newWhyDescendantTestApp(t)
	sess, err := s.Create(context.Background(), "root waiting on its own bash job")
	require.NoError(t, err)
	addFinishedAssistant(t, m, sess.ID, message.FinishReasonEndTurn)
	claimOwnJob(t, a, sess.ID, "bash-1")

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &buf))
	out := buf.String()

	require.Equal(t, "status: running", strings.SplitN(out, "\n", 2)[0])
	// CHANGED STRING (R-ACT-2): the reason is the classifier's Description,
	// which names the count of running jobs, not the old per-clause render.
	require.Contains(t, out, "waiting on 1 running job(s)")
	require.NotContains(t, out, "status: at rest")

	finishOwnJob(t, a, sess.ID, "bash-1")
	buf.Reset()
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &buf))
	require.Equal(t, "status: at rest", strings.SplitN(buf.String(), "\n", 2)[0],
		"a finished job no longer keeps the session running")
}

func TestExplainSessionStatus_DeadPIDLockWithOwnJobIsRunningNotCrashed(t *testing.T) {
	// F11 env pin: legacy-render first line; see TestExplainSessionStatus_AtRestOwnRunningJobIsRunning.
	t.Setenv("RUSH_BATCH_RUNTIME", "false")
	// CHANGED VERDICT (R-ACT-2, D1): the pre-rework rule kept this crashed
	// ("never downgrade a crash"); live work now outranks the dead recorded
	// PID, which becomes the "(stale lock)" annotation.
	a, s, m, _, dataDir := newWhyDescendantTestApp(t)
	sess, err := s.Create(context.Background(), "stale-lock root waiting on its own job")
	require.NoError(t, err)
	backDateLock(t, writeLockFileAt(t, dataDir, sess.ID, 999999))
	addFinishedAssistant(t, m, sess.ID, message.FinishReasonEndTurn)
	claimOwnJob(t, a, sess.ID, "bash-1")

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &buf))
	out := buf.String()

	require.Equal(t, "status: running (stale lock)", strings.SplitN(out, "\n", 2)[0])
	require.Contains(t, out, "waiting on 1 running job(s)")
	require.Contains(t, out, "dead PID 999999")
	require.NotContains(t, out, "status: done")
}

func TestExplainSessionStatus_DeadPIDLockWithoutJobIsCrashed(t *testing.T) {
	// F11 env pin: legacy-render first line; see TestExplainSessionStatus_AtRestOwnRunningJobIsRunning.
	t.Setenv("RUSH_BATCH_RUNTIME", "false")
	// D3: with the job finished there is no live work; the recorded dead PID
	// (the holder never reached release) is a crash even with an end_turn
	// finish from a previous turn.
	a, s, m, _, dataDir := newWhyDescendantTestApp(t)
	sess, err := s.Create(context.Background(), "crashed root, job finished")
	require.NoError(t, err)
	backDateLock(t, writeLockFileAt(t, dataDir, sess.ID, 999999))
	addFinishedAssistant(t, m, sess.ID, message.FinishReasonError)
	claimOwnJob(t, a, sess.ID, "bash-1")
	finishOwnJob(t, a, sess.ID, "bash-1")

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &buf))
	require.Equal(t, "status: crashed", strings.SplitN(buf.String(), "\n", 2)[0],
		"a dead holder with nothing live is the session's own crash, whatever the last finish")
}

// TestListStatus_OwnJobShapes pins the list STATUS column across the same
// shapes, through the one classifier (the deleted per-command layer's
// replacement). CHANGED VERDICTS vs the deleted layer's table: dead-PID +
// live job is running (D1, was "never downgrade a crash"); a dead-host job
// with nothing live is crashed (D5, was done); delegating + own job stays
// delegating (D2, the old layer's own-jobs-last order said running).
//
// Revert-check: any local re-ranking of these shapes in a reader breaks its
// row here while the other rows still pass.
func TestListStatus_OwnJobShapes(t *testing.T) {
	// F11 env pin: pins the dead-host crash fact, which the batch authority
	// deliberately has no equivalent of (dead hosts stay live until recovery).
	t.Setenv("RUSH_BATCH_RUNTIME", "false")
	a, s, _, _, dataDir := newWhyDescendantTestApp(t)
	ctx := context.Background()
	mk := func(title string) session.Session {
		sess, err := s.Create(ctx, title)
		require.NoError(t, err)
		return sess
	}
	blank, deadPID, deadHost, withJob := mk("blank"), mk("dead-pid"), mk("dead-host"), mk("both")

	claimOwnJob(t, a, blank.ID, "bash-1")
	claimOwnJob(t, a, deadPID.ID, "bash-1")
	backDateLock(t, writeLockFileAt(t, dataDir, deadPID.ID, 999999))

	deadStore := session.NewAsyncJobStore(mustDBConn(t, dataDir), dataDir, 4242, "dead")
	t.Cleanup(func() { _ = deadStore.Close(ctx) })
	_, err := deadStore.Claim(ctx, session.ClaimParams{Owner: deadHost.ID, ToolCallID: "bash-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, deadStore.SimulateCrashForTest())

	claimOwnJob(t, a, withJob.ID, "bash-1")
	other := mk("other")
	claimDelegation(t, a, withJob.ID, "agent-1", other.ID)

	acts, err := a.SessionActivityBatch(ctx, []string{blank.ID, deadPID.ID, deadHost.ID, withJob.ID})
	require.NoError(t, err)
	for _, act := range acts.ByID {
		require.Empty(t, act.Facts.Unreadable)
	}
	require.Equal(t, "running", listStatus(acts.ByID[blank.ID].Verdict))
	require.Equal(t, "running", listStatus(acts.ByID[deadPID.ID].Verdict), "D1: the live job outranks the dead PID")
	require.Equal(t, "crashed", listStatus(acts.ByID[deadHost.ID].Verdict), "D5: a dead-host row with nothing live is a crash")
	require.Equal(t, "delegating", listStatus(acts.ByID[withJob.ID].Verdict), "D2: the delegation is the Kind, the job a wait")
	require.Contains(t, acts.ByID[withJob.ID].Verdict.WaitingOn, session.WaitTasks)
}

// TestSessionsListCmdRun_OwnRunningJobIsRunningNotDone drives the real
// `sessions list`: a parent with a dead-PID lock and a clean end_turn that
// owns a running plain job must read "running", and read "crashed" once the
// job is terminal (D3: a dead PID with nothing live is a crash -- the old
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

	store := a.AsyncJobStore()
	require.NotNil(t, store)
	claimOwnJob(t, a, parent.ID, "bash-1")
	// Keep the store (and its host lock) alive across the seed App's
	// Shutdown -- same dance as the delegating variant of this test.
	a.SetAsyncJobStoreForTest(nil)
	defer func() { _ = store.Close(context.Background()) }()
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
	require.Contains(t, phase1, "running", "a root waiting on its own running job is working, not done (D1)")
	require.NotContains(t, phase1, "done")

	freshConn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	if config.BatchRuntimeEnabled() {
		// Flag ON: settle the batch root with the finishOwnJob-style raw
		// UPDATE (the seed app is shut down; write through a fresh conn).
		_, err = freshConn.ExecContext(ctx,
			`UPDATE batch_nodes SET state = 'completed', control_state = 'none', updated_at = ?
			 WHERE owner_session_id = ? AND (tool_call_id = ? OR root_id IN
				 (SELECT root_id FROM batch_nodes WHERE owner_session_id = ? AND tool_call_id = ?))`,
			time.Now().Unix(), parent.ID, "bash-1", parent.ID, "bash-1")
		require.NoError(t, err)
	} else {
		// Flag OFF: the fresh-writer Transition exactly as before (the seed
		// app is shut down and its store detached, so `a` cannot be used).
		writer := session.NewAsyncJobStore(freshConn, dataDir, 1001, "test-writer")
		_, err = writer.Transition(ctx, session.TransitionParams{
			Owner: parent.ID, ToolCallID: "bash-1", State: "completed", NoticeKind: "completed", Wake: true,
		})
		require.NoError(t, err)
	}

	phase2 := rowFor(runList())
	// CHANGED VERDICT (R-ACT-2, D3/D4): with the job terminal there is no
	// live work; the recorded dead PID is a crash, and the end_turn finish
	// is not an end signal. The old expectation was "done".
	require.Contains(t, phase2, "crashed")
	require.NotContains(t, phase2, "running")
}
