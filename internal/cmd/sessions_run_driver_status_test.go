package cmd

// R5C-5: a session whose live `rush run` loop is between turns (debt pending,
// a paced retry ahead) holds no session lock and has no running row; the
// durable driver marker (session_drivers + host liveness, unknown = alive) is
// the only cross-process fact that says the scope is open (ASYNC-02). `sessions
// why` and `sessions list` must not report such a session as at rest / done.

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// newRunDriverTestApp is newWhyDescendantTestApp with the App's DB handle
// wired (the driver-marker reader reads through it).
func newRunDriverTestApp(t *testing.T) (a *app.App, s session.Service, m message.Service, store *session.AsyncJobStore, dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	q := db.New(conn)
	s = session.NewService(q, conn)
	m = message.NewService(q)
	store = session.NewAsyncJobStore(conn, dataDir, 4321, "rush-run")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	a = &app.App{Messages: m, Sessions: s, DB: func() *sql.DB { return conn }}
	a.SetAsyncJobStoreForTest(store)
	return a, s, m, store, dataDir
}

func verdictLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }

// The loop's Drain failed with a provider 5xx: attempt counted, gate paced, no
// lock between turns, no running row. The marker on a live host keeps the
// session running; releasing it (the loop exited) gives the at-rest verdict
// back.
//
// Revert-check: dropping the driver consultation from explainSessionStatus
// makes the first assertion read "status: at rest".
func TestExplainSessionStatus_LiveRunDriverBetweenTurnsIsRunning(t *testing.T) {
	t.Parallel()
	a, s, m, store, dataDir := newRunDriverTestApp(t)
	ctx := context.Background()
	sess, err := s.Create(ctx, "driven between turns")
	require.NoError(t, err)
	addFinishedAssistant(t, m, sess.ID, message.FinishReasonEndTurn)
	require.NoError(t, store.InsertSessionNotice(ctx, sess.ID, "job_result", "a job finished", true, ""))
	require.NoError(t, store.ClaimSessionDriver(ctx, sess.ID))

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, sess.ID, &buf))
	out := buf.String()

	require.Equal(t, "status: running", verdictLine(out))
	require.Contains(t, out, "`rush run`")
	require.Contains(t, out, "PID 4321")
	require.Contains(t, out, "reaction is owed", "the reason names why the loop is waiting")
	require.NotContains(t, out, "not running, not crashed")
	require.NotContains(t, out, "session is idle")

	require.NoError(t, store.ReleaseSessionDriver(ctx, sess.ID))
	buf.Reset()
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, sess.ID, &buf))
	require.Equal(t, "status: at rest", verdictLine(buf.String()), "no marker: the loop is gone")
}

// The stale-lock shape (a previous turn's lock left behind, clean end_turn):
// with a live driver it is running, not done; a crashed session stays crashed.
//
// Revert-check: as above; the first line reads "status: done (stale lock)".
func TestExplainSessionStatus_LiveRunDriverStaleLockAndCrashed(t *testing.T) {
	t.Parallel()
	a, s, m, store, dataDir := newRunDriverTestApp(t)
	ctx := context.Background()

	stale, err := s.Create(ctx, "stale lock, driven")
	require.NoError(t, err)
	backDateLock(t, writeLockFileAt(t, dataDir, stale.ID, 999999))
	addFinishedAssistant(t, m, stale.ID, message.FinishReasonEndTurn)
	require.NoError(t, store.ClaimSessionDriver(ctx, stale.ID))

	crashed, err := s.Create(ctx, "crashed, driven")
	require.NoError(t, err)
	backDateLock(t, writeLockFileAt(t, dataDir, crashed.ID, 999999))
	addFinishedAssistant(t, m, crashed.ID, message.FinishReasonError)
	require.NoError(t, store.ClaimSessionDriver(ctx, crashed.ID))

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, stale.ID, &buf))
	require.Equal(t, "status: running (stale lock)", verdictLine(buf.String()))
	require.NotContains(t, buf.String(), "Treat as done")

	buf.Reset()
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, crashed.ID, &buf))
	require.Equal(t, "status: crashed", verdictLine(buf.String()), "a dead lock holder without a clean finish is the session's own crash")
}

// A marker naming a dead host (a crashed `rush run` that never released it)
// keeps nothing open.
//
// Revert-check: treating every marker as live makes the verdict "running".
func TestExplainSessionStatus_DeadHostRunDriverIsAtRest(t *testing.T) {
	t.Parallel()
	a, s, m, _, dataDir := newRunDriverTestApp(t)
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	sess, err := s.Create(ctx, "driver crashed")
	require.NoError(t, err)
	addFinishedAssistant(t, m, sess.ID, message.FinishReasonEndTurn)
	dead := session.NewAsyncJobStore(conn, dataDir, 4242, "dead-loop")
	require.NoError(t, dead.ClaimSessionDriver(ctx, sess.ID))
	require.NoError(t, dead.SimulateCrashForTest())

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, sess.ID, &buf))
	require.Equal(t, "status: at rest", verdictLine(buf.String()))
}

// TestMarkLiveRunDrivers pins the list-side promotion: done/at-rest sessions
// with a live driver become running; crashed, delegating and running keep
// their own signal; a dead host's marker does not promote.
//
// Revert-check: making markLiveRunDrivers return statusByID untouched fails
// the promotion rows.
func TestMarkLiveRunDrivers(t *testing.T) {
	t.Parallel()
	a, s, _, store, dataDir := newRunDriverTestApp(t)
	ctx := context.Background()
	mk := func(title string) session.Session {
		sess, err := s.Create(ctx, title)
		require.NoError(t, err)
		return sess
	}
	done, blank, crashed, delegating, running, deadHost, undriven := mk("done"), mk("blank"), mk("crashed"), mk("delegating"), mk("running"), mk("dead-host"), mk("undriven")
	for _, sess := range []session.Session{done, blank, crashed, delegating, running} {
		require.NoError(t, store.ClaimSessionDriver(ctx, sess.ID))
	}
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	dead := session.NewAsyncJobStore(conn, dataDir, 4242, "dead")
	require.NoError(t, dead.ClaimSessionDriver(ctx, deadHost.ID))
	require.NoError(t, dead.SimulateCrashForTest())

	sessions := []session.Session{done, blank, crashed, delegating, running, deadHost, undriven}
	got := markLiveRunDrivers(ctx, a, sessions, map[string]string{
		done.ID: "done", crashed.ID: "crashed", delegating.ID: "delegating", running.ID: "running", deadHost.ID: "done",
	})
	require.Equal(t, "running", got[done.ID])
	require.Equal(t, "running", got[blank.ID])
	require.Equal(t, "crashed", got[crashed.ID], "never downgrade a crash")
	require.Equal(t, "delegating", got[delegating.ID])
	require.Equal(t, "running", got[running.ID])
	require.Equal(t, "done", got[deadHost.ID], "a dead host's marker keeps nothing open")
	require.Empty(t, got[undriven.ID])

	require.Nil(t, markLiveRunDrivers(ctx, nil, sessions, nil), "nil app is a no-op")
	fromNil := markLiveRunDrivers(ctx, a, sessions, nil)
	require.Equal(t, "running", fromNil[blank.ID], "a nil status map (unreadable locks dir) is created on promotion")
}

// The real `sessions list`: a session with a stale lock and a clean end_turn
// (the "done" shape) driven by a live loop reads running, and done again once
// the loop released its marker.
//
// Revert-check: dropping the markLiveRunDrivers call from the list command
// fails phase 1 (the row reads "done").
func TestSessionsListCmdRun_LiveRunDriverIsRunningNotDone(t *testing.T) {
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()

	parent, err := a.Sessions.CreateWithID(ctx, "list-run-driver-root", "root driven by a loop")
	require.NoError(t, err)
	lockDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	parentLock := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(parent.ID)+".lock")
	require.NoError(t, os.WriteFile(parentLock, []byte("999999\n"), 0o644))
	backDateLock(t, parentLock)
	assistant, err := a.Messages.Create(ctx, parent.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "job started"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, assistant))

	store := a.AsyncJobStore()
	require.NotNil(t, store)
	require.NoError(t, store.ClaimSessionDriver(ctx, parent.ID))
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
	require.Contains(t, phase1, "running", "a session driven by a live `rush run` loop is working, not done")
	require.NotContains(t, phase1, "done")

	// The loop exits: its marker is deleted (the seed App's own connection is
	// closed by now, so delete through a fresh one).
	freshConn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	_, err = freshConn.ExecContext(ctx, `DELETE FROM session_drivers WHERE session_id = ?`, parent.ID)
	require.NoError(t, err)
	phase2 := rowFor(runList())
	require.Contains(t, phase2, "done", "the loop released its marker: the clean-exit reclassification applies again")
	require.NotContains(t, phase2, "running")
}
