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

func claimOwnJob(t *testing.T, store *session.AsyncJobStore, owner, toolCallID string) {
	t.Helper()
	_, err := store.Claim(context.Background(), session.ClaimParams{
		Owner: owner, ToolCallID: toolCallID, Kind: session.JobKindCommand, Input: "sleep 60", ToolName: "bash",
	})
	require.NoError(t, err)
}

func finishOwnJob(t *testing.T, store *session.AsyncJobStore, owner, toolCallID string) {
	t.Helper()
	_, err := store.Transition(context.Background(), session.TransitionParams{
		Owner: owner, ToolCallID: toolCallID, State: "completed", NoticeKind: "completed", Wake: true,
	})
	require.NoError(t, err)
}

// Revert-check: any re-introduction of a local own-job promotion (or its
// omission from the classifier) flips the first line back to
// "status: at rest" and fails the first assertion.
func TestExplainSessionStatus_AtRestOwnRunningJobIsRunning(t *testing.T) {
	t.Parallel()
	a, s, m, store, dataDir := newWhyDescendantTestApp(t)
	sess, err := s.Create(context.Background(), "root waiting on its own bash job")
	require.NoError(t, err)
	addFinishedAssistant(t, m, sess.ID, message.FinishReasonEndTurn)
	claimOwnJob(t, store, sess.ID, "bash-1")

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &buf))
	out := buf.String()

	require.Equal(t, "status: running", strings.SplitN(out, "\n", 2)[0])
	// CHANGED STRING (R-ACT-2): the reason is the classifier's Description,
	// which names the count of running jobs, not the old per-clause render.
	require.Contains(t, out, "waiting on 1 running job(s)")
	require.NotContains(t, out, "status: at rest")

	finishOwnJob(t, store, sess.ID, "bash-1")
	buf.Reset()
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &buf))
	require.Equal(t, "status: at rest", strings.SplitN(buf.String(), "\n", 2)[0],
		"a finished job no longer keeps the session running")
}

func TestExplainSessionStatus_DeadPIDLockWithOwnJobIsRunningNotCrashed(t *testing.T) {
	t.Parallel()
	// CHANGED VERDICT (R-ACT-2, D1): the pre-rework rule kept this crashed
	// ("never downgrade a crash"); live work now outranks the dead recorded
	// PID, which becomes the "(stale lock)" annotation.
	a, s, m, store, dataDir := newWhyDescendantTestApp(t)
	sess, err := s.Create(context.Background(), "stale-lock root waiting on its own job")
	require.NoError(t, err)
	backDateLock(t, writeLockFileAt(t, dataDir, sess.ID, 999999))
	addFinishedAssistant(t, m, sess.ID, message.FinishReasonEndTurn)
	claimOwnJob(t, store, sess.ID, "bash-1")

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &buf))
	out := buf.String()

	require.Equal(t, "status: running (stale lock)", strings.SplitN(out, "\n", 2)[0])
	require.Contains(t, out, "waiting on 1 running job(s)")
	require.Contains(t, out, "dead PID 999999")
	require.NotContains(t, out, "status: done")
}

func TestExplainSessionStatus_DeadPIDLockWithoutJobIsCrashed(t *testing.T) {
	t.Parallel()
	// D3: with the job finished there is no live work; the recorded dead PID
	// (the holder never reached release) is a crash even with an end_turn
	// finish from a previous turn.
	a, s, m, store, dataDir := newWhyDescendantTestApp(t)
	sess, err := s.Create(context.Background(), "crashed root, job finished")
	require.NoError(t, err)
	backDateLock(t, writeLockFileAt(t, dataDir, sess.ID, 999999))
	addFinishedAssistant(t, m, sess.ID, message.FinishReasonError)
	claimOwnJob(t, store, sess.ID, "bash-1")
	finishOwnJob(t, store, sess.ID, "bash-1")

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
	t.Parallel()
	a, s, _, store, dataDir := newWhyDescendantTestApp(t)
	ctx := context.Background()
	mk := func(title string) session.Session {
		sess, err := s.Create(ctx, title)
		require.NoError(t, err)
		return sess
	}
	blank, deadPID, deadHost, withJob := mk("blank"), mk("dead-pid"), mk("dead-host"), mk("both")

	claimOwnJob(t, store, blank.ID, "bash-1")
	claimOwnJob(t, store, deadPID.ID, "bash-1")
	backDateLock(t, writeLockFileAt(t, dataDir, deadPID.ID, 999999))

	deadStore := session.NewAsyncJobStore(mustDBConn(t, dataDir), dataDir, 4242, "dead")
	t.Cleanup(func() { _ = deadStore.Close(ctx) })
	_, err := deadStore.Claim(ctx, session.ClaimParams{Owner: deadHost.ID, ToolCallID: "bash-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, deadStore.SimulateCrashForTest())

	claimOwnJob(t, store, withJob.ID, "bash-1")
	other := mk("other")
	claimDelegation(t, store, withJob.ID, "agent-1", other.ID)

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
	claimOwnJob(t, store, parent.ID, "bash-1")
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
	writer := session.NewAsyncJobStore(freshConn, dataDir, 1001, "test-writer")
	finishOwnJob(t, writer, parent.ID, "bash-1")

	phase2 := rowFor(runList())
	// CHANGED VERDICT (R-ACT-2, D3/D4): with the job terminal there is no
	// live work; the recorded dead PID is a crash, and the end_turn finish
	// is not an end signal. The old expectation was "done".
	require.Contains(t, phase2, "crashed")
	require.NotContains(t, phase2, "running")
}
