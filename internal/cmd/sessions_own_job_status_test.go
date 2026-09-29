package cmd

// A root whose scope is open only because of its OWN running plain
// background job (bash/run_command; no delegation row, no lock between turns)
// used to headline "done" / "at rest" in `sessions list` and `sessions why`,
// because only delegation rows promoted a session. It is working: both
// commands now report "running" (the existing vocabulary), and `sessions why`
// names the job. A crashed root stays crashed (never-downgrade rule).

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

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

// Revert-check (all `why` tests below): removed the LiveOwnJobs consultation
// from explainSessionStatus -- the running-verdict assertions FAILED (the
// first line read "status: at rest" / "status: done (stale lock)").
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
	require.Contains(t, out, "its own background job bash-1")
	require.Contains(t, out, "NOT done")
	require.NotContains(t, out, "status: at rest")
	require.NotContains(t, out, "session is idle")

	finishOwnJob(t, store, sess.ID, "bash-1")
	buf.Reset()
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &buf))
	require.Equal(t, "status: at rest", strings.SplitN(buf.String(), "\n", 2)[0],
		"a finished job no longer keeps the session running")
}

func TestExplainSessionStatus_StaleLockOwnRunningJobIsRunning(t *testing.T) {
	t.Parallel()
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
	require.Contains(t, out, "its own background job bash-1")
	require.NotContains(t, out, "Treat as done")
	require.NotContains(t, out, "status: done")
}

func TestExplainSessionStatus_CrashedRootWithOwnJobStaysCrashed(t *testing.T) {
	t.Parallel()
	a, s, m, store, dataDir := newWhyDescendantTestApp(t)
	sess, err := s.Create(context.Background(), "crashed root with a live own job")
	require.NoError(t, err)
	backDateLock(t, writeLockFileAt(t, dataDir, sess.ID, 999999))
	addFinishedAssistant(t, m, sess.ID, message.FinishReasonError) // no clean finish
	claimOwnJob(t, store, sess.ID, "bash-1")

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, sess.ID, &buf))
	require.Equal(t, "status: crashed", strings.SplitN(buf.String(), "\n", 2)[0],
		"a dead holder without a clean finish is the session's own crash, whatever job it owns")
}

// TestMarkRunningOwnJobs pins the list-side promotion: done/at-rest roots with
// a live own job become running; crashed, delegating and running keep their
// own signal; a job on a dead host does not promote.
//
// Revert-check performed: made markRunningOwnJobs return statusByID untouched
// -- the promotion subtests FAILED.
func TestMarkRunningOwnJobs(t *testing.T) {
	t.Parallel()
	a, s, _, store, dataDir := newWhyDescendantTestApp(t)
	ctx := context.Background()
	mk := func(title string) session.Session {
		sess, err := s.Create(ctx, title)
		require.NoError(t, err)
		return sess
	}
	done, blank, crashed, delegating, running, deadHost := mk("done"), mk("blank"), mk("crashed"), mk("delegating"), mk("running"), mk("dead-host")
	for _, sess := range []session.Session{done, blank, crashed, delegating, running} {
		claimOwnJob(t, store, sess.ID, "bash-1")
	}
	// A running row whose host is dead: crashed store, row stays running.
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	dead := session.NewAsyncJobStore(conn, dataDir, 4242, "dead")
	_, err = dead.Claim(ctx, session.ClaimParams{Owner: deadHost.ID, ToolCallID: "bash-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, dead.SimulateCrashForTest())

	sessions := []session.Session{done, blank, crashed, delegating, running, deadHost}
	got := markRunningOwnJobs(ctx, a, sessions, map[string]string{
		done.ID: "done", crashed.ID: "crashed", delegating.ID: "delegating", running.ID: "running", deadHost.ID: "done",
	})
	require.Equal(t, "running", got[done.ID], "a done root with a live own job is running")
	require.Equal(t, "running", got[blank.ID], "an at-rest root with a live own job is running")
	require.Equal(t, "crashed", got[crashed.ID], "never downgrade a crash")
	require.Equal(t, "delegating", got[delegating.ID])
	require.Equal(t, "running", got[running.ID])
	require.Equal(t, "done", got[deadHost.ID], "a job on a dead host keeps nothing open")

	require.Nil(t, markRunningOwnJobs(ctx, nil, sessions, nil), "nil app is a no-op")
	fromNil := markRunningOwnJobs(ctx, a, sessions, nil)
	require.Equal(t, "running", fromNil[blank.ID], "a nil status map (unreadable locks dir) is created on promotion")
}

// TestSessionsListCmdRun_OwnRunningJobIsRunningNotDone drives the real
// `sessions list`: a parent with a stale lock and a clean end_turn (the "done"
// shape) that owns a running plain job must read "running", and read "done"
// again once the job is terminal.
//
// Revert-check performed: dropped the markRunningOwnJobs call from the list
// command -- phase 1 FAILED (the row read "done").
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
	require.Contains(t, phase1, "running", "a root waiting on its own running job is working, not done")
	require.NotContains(t, phase1, "done")
	require.NotContains(t, phase1, "delegating", "it has no delegation")

	freshConn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	writer := session.NewAsyncJobStore(freshConn, dataDir, 1001, "test-writer")
	finishOwnJob(t, writer, parent.ID, "bash-1")

	phase2 := rowFor(runList())
	require.Contains(t, phase2, "done", "with the job terminal the clean-exit reclassification applies again")
	require.NotContains(t, phase2, "running")
}
