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
	a.SetDataDirForTest(dataDir)
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

// The stale-lock shape a turn leaves behind (an EMPTY back-dated lock after a
// clean release, clean end_turn): with a live driver it is running, not done.
// A lock recording another process's dead PID with an error finish is that
// process's own crash and stays crashed, driver or not (the empty-lock error
// shape is TestExplainSessionStatus_ReleasedLockErrorFinishLiveDriverIsRunning).
//
// Revert-check: as above; the first line reads "status: done (stale lock)".
func TestExplainSessionStatus_LiveRunDriverStaleLockAndCrashed(t *testing.T) {
	t.Parallel()
	a, s, m, store, dataDir := newRunDriverTestApp(t)
	ctx := context.Background()

	stale, err := s.Create(ctx, "stale lock, driven")
	require.NoError(t, err)
	releasedLock(t, dataDir, stale.ID)
	addFinishedAssistant(t, m, stale.ID, message.FinishReasonEndTurn)
	require.NoError(t, store.ClaimSessionDriver(ctx, stale.ID))

	crashed, err := s.Create(ctx, "crashed, driven")
	require.NoError(t, err)
	backDateLock(t, writeLockFileAt(t, dataDir, crashed.ID, 999999))
	addFinishedAssistant(t, m, crashed.ID, message.FinishReasonError)
	require.NoError(t, store.ClaimSessionDriver(ctx, crashed.ID))

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, stale.ID, &buf))
	// CHANGED VERDICT (R-ACT-2, D1): a released lock (no recorded PID) under a live
	// driver is plainly running; the old "(stale lock)" suffix came from the
	// finish-reason reclassification layer that no longer exists.
	require.Equal(t, "status: running", verdictLine(buf.String()))
	require.NotContains(t, buf.String(), "Treat as done")

	buf.Reset()
	// CHANGED VERDICT (R-ACT-2, D1): the live driver outranks the recorded
	// dead PID, which becomes the stale-lock annotation. The old expectation
	// was "crashed" (never-downgrade).
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, crashed.ID, &buf))
	require.Equal(t, "status: running (stale lock)", verdictLine(buf.String()))
	require.Contains(t, buf.String(), "dead PID 999999")
}

// A marker naming a dead host (a crashed `rush run` that never released it)
// is a crash fact (D5), not a live driver.
//
// CHANGED VERDICT (R-ACT-2, D5): the pre-rework verdict was "at rest"; the
// classifier now reports the unreachable-marker shape as crashed.
//
// Revert-check: treating every marker as live makes the verdict "running".
func TestExplainSessionStatus_DeadHostRunDriverIsCrashed(t *testing.T) {
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
	require.Equal(t, "status: crashed", verdictLine(buf.String()))
}

// TestListStatus_DriverShapes pins the list STATUS column across driver
// shapes through the one classifier (the deleted per-command layer's
// replacement). CHANGED VERDICTS vs the deleted layer's table: a dead-PID
// lock plus a live driver is running (D1, was "never downgrade a crash");
// a dead host's marker with nothing live is crashed (D5, was done);
// delegating + driver stays delegating (D2).
//
// Revert-check: any local re-ranking breaks its row while the others pass.
func TestListStatus_DriverShapes(t *testing.T) {
	t.Parallel()
	a, s, _, store, dataDir := newRunDriverTestApp(t)
	ctx := context.Background()
	mk := func(title string) session.Session {
		sess, err := s.Create(ctx, title)
		require.NoError(t, err)
		return sess
	}
	done, blank, deadPID, delegating, deadHost, undriven := mk("done"), mk("blank"), mk("dead-pid"), mk("delegating"), mk("dead-host"), mk("undriven")
	for _, sess := range []session.Session{done, blank, deadPID, delegating} {
		require.NoError(t, store.ClaimSessionDriver(ctx, sess.ID))
	}
	// The dead-PID shape: a recorded PID that is not alive (D1's annotation
	// input). The delegating shape needs a live delegation row.
	backDateLock(t, writeLockFileAt(t, dataDir, deadPID.ID, 999999))
	other := mk("other")
	claimDelegation(t, store, delegating.ID, "agent-1", other.ID)

	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	dead := session.NewAsyncJobStore(conn, dataDir, 4242, "dead")
	require.NoError(t, dead.ClaimSessionDriver(ctx, deadHost.ID))
	require.NoError(t, dead.SimulateCrashForTest())

	ids := []string{done.ID, blank.ID, deadPID.ID, delegating.ID, deadHost.ID, undriven.ID}
	acts, actErr := a.SessionActivityBatch(ctx, ids)
	require.NoError(t, actErr)
	got := map[string]string{}
	for id, act := range acts.ByID {
		require.Empty(t, act.Facts.Unreadable, id)
		got[id] = listStatus(act.Verdict)
	}
	require.Equal(t, "running", got[done.ID])
	require.Equal(t, "running", got[blank.ID])
	require.Equal(t, "running", got[deadPID.ID], "D1: the live driver outranks the dead recorded PID")
	require.Equal(t, "delegating", got[delegating.ID], "D2: the delegation is the Kind, the driver a fact")
	require.Equal(t, "crashed", got[deadHost.ID], "D5: a dead host's marker with nothing live is a crash")
	require.Empty(t, got[undriven.ID])
}

// The real `sessions list`: a session with a stale lock and a clean end_turn
// (the "done" shape) driven by a live loop reads running, and done again once
// the loop released its marker.
//
// Revert-check: dropping the driver fact from the classifier
// fails phase 1 (the row reads "done").
func TestSessionsListCmdRun_LiveRunDriverIsRunningNotDone(t *testing.T) {
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()

	parent, err := a.Sessions.CreateWithID(ctx, "list-run-driver-root", "root driven by a loop")
	require.NoError(t, err)
	lockDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	parentLock := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(parent.ID)+".lock")
	require.NoError(t, os.WriteFile(parentLock, nil, 0o644)) // a clean release leaves an empty file
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
	// CHANGED VERDICT (R-ACT-2, D4): with the marker gone there is no live
	// work and no recorded ended_reason (the finish is not an end signal),
	// so the row reads at rest (blank), not "done".
	require.NotContains(t, phase2, "running")
	require.NotContains(t, phase2, "done")
}
