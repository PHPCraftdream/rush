package sessstate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/sesskit"
	"github.com/PHPCraftdream/rush/internal/session"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

func stateRunDriverTestApp(t *testing.T) (*app.App, string, *sessionhosts.HostStore) {
	t.Helper()
	a, dataDir, store := cmdtest.NewActivityCmdApp(t)
	return a, dataDir, store
}

func stateSessionStatuses(t *testing.T, a *app.App, ids []string) (map[string]string, app.SessionActivityBatchResult) {
	t.Helper()
	activities, err := a.SessionActivityBatch(context.Background(), ids)
	require.NoError(t, err)
	statuses, _ := sesskit.ListStatusesWithAwaiting(context.Background(), sesskit.SessionQuestionReaderForApp(a), activities, ids)
	return statuses, activities
}

// TestExplainSessionStatus_LiveRunDriverBetweenTurns verifies the internal
// state explanation reads the durable driver marker while no session lock is held.
func TestExplainSessionStatus_LiveRunDriverBetweenTurnsIsRunning(t *testing.T) {
	a, dataDir, store := stateRunDriverTestApp(t)
	ctx := context.Background()
	sess, err := a.Sessions.Create(ctx, "driven between turns")
	require.NoError(t, err)
	cmdtest.AddFinishedAssistant(t, a.Messages, sess.ID, message.FinishReasonEndTurn)
	require.NoError(t, session.NewSessionNoticeStore(a.DB()).InsertSessionNotice(ctx, sess.ID, "job_result", "a job finished", true, ""))
	require.NoError(t, store.ClaimSessionDriver(ctx, sess.ID))

	var out bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, sess.ID, &out))
	require.Equal(t, "status: running", strings.SplitN(out.String(), "\n", 2)[0])
	require.Contains(t, out.String(), "`rush run`")
	require.Contains(t, out.String(), fmt.Sprintf("PID %d", os.Getpid()))
	require.Contains(t, out.String(), "reaction is owed", "the reason names why the loop is waiting")
	require.NotContains(t, out.String(), "not running, not crashed")
	require.NotContains(t, out.String(), "session is idle")

	require.NoError(t, store.ReleaseSessionDriver(ctx, sess.ID))
	out.Reset()
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, sess.ID, &out))
	require.Equal(t, "status: at rest", strings.SplitN(out.String(), "\n", 2)[0])
}

func TestExplainSessionStatus_LiveRunDriverStaleLockAndCrashed(t *testing.T) {
	a, dataDir, store := stateRunDriverTestApp(t)
	ctx := context.Background()
	stale, err := a.Sessions.Create(ctx, "stale lock, driven")
	require.NoError(t, err)
	cmdtest.ReleasedLock(t, dataDir, stale.ID)
	cmdtest.AddFinishedAssistant(t, a.Messages, stale.ID, message.FinishReasonEndTurn)
	require.NoError(t, store.ClaimSessionDriver(ctx, stale.ID))

	crashed, err := a.Sessions.Create(ctx, "crashed, driven")
	require.NoError(t, err)
	cmdtest.BackDateLock(t, cmdtest.WriteLockFileAt(t, dataDir, crashed.ID, 999999))
	cmdtest.AddFinishedAssistant(t, a.Messages, crashed.ID, message.FinishReasonError)
	require.NoError(t, store.ClaimSessionDriver(ctx, crashed.ID))

	var out bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, stale.ID, &out))
	require.Equal(t, "status: running", strings.SplitN(out.String(), "\n", 2)[0])
	require.NotContains(t, out.String(), "Treat as done")
	out.Reset()
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, crashed.ID, &out))
	require.Equal(t, "status: running (stale lock)", strings.SplitN(out.String(), "\n", 2)[0])
	require.Contains(t, out.String(), "dead PID 999999")
}

func TestExplainSessionStatus_DeadHostRunDriverIsCrashed(t *testing.T) {
	a, dataDir, _ := stateRunDriverTestApp(t)
	ctx := context.Background()
	sess, err := a.Sessions.Create(ctx, "driver crashed")
	require.NoError(t, err)
	cmdtest.AddFinishedAssistant(t, a.Messages, sess.ID, message.FinishReasonEndTurn)
	dead := cmdtest.NewDeadHostStore(t, dataDir, 4242, "dead-loop")
	require.NoError(t, dead.ClaimSessionDriver(ctx, sess.ID))
	require.NoError(t, dead.SimulateCrashForTest())

	var out bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, sess.ID, &out))
	require.Equal(t, "status: crashed", strings.SplitN(out.String(), "\n", 2)[0])
}

// TestListStatus_DriverShapes checks the classifier adapter; the command-level
// real scan List path is kept as a separate test below.
func TestListStatus_DriverShapes(t *testing.T) {
	a, dataDir, store := stateRunDriverTestApp(t)
	ctx := context.Background()
	mk := func(title string) coresession.Session {
		sess, err := a.Sessions.Create(ctx, title)
		require.NoError(t, err)
		return sess
	}
	done, blank, deadPID, delegating, deadHost, undriven := mk("done"), mk("blank"), mk("dead-pid"), mk("delegating"), mk("dead-host"), mk("undriven")
	for _, sess := range []coresession.Session{done, blank, deadPID, delegating} {
		require.NoError(t, store.ClaimSessionDriver(ctx, sess.ID))
	}
	cmdtest.BackDateLock(t, cmdtest.WriteLockFileAt(t, dataDir, deadPID.ID, 999999))
	other := mk("other")
	cmdtest.ClaimDelegation(t, a, delegating.ID, "agent-1", other.ID)
	dead := cmdtest.NewDeadHostStore(t, dataDir, 4242, "dead")
	require.NoError(t, dead.ClaimSessionDriver(ctx, deadHost.ID))
	require.NoError(t, dead.SimulateCrashForTest())

	ids := []string{done.ID, blank.ID, deadPID.ID, delegating.ID, deadHost.ID, undriven.ID}
	statuses, activities := stateSessionStatuses(t, a, ids)
	for id, act := range activities.ByID {
		require.Empty(t, act.Facts.Unreadable, id)
	}
	require.Equal(t, "running", statuses[done.ID])
	require.Equal(t, "running", statuses[blank.ID])
	require.Equal(t, "running", statuses[deadPID.ID], "live driver outranks dead recorded PID")
	require.Equal(t, "delegating", statuses[delegating.ID], "delegation stays the kind")
	require.Equal(t, "crashed", statuses[deadHost.ID], "dead host marker is a crash")
	require.Empty(t, statuses[undriven.ID])
}

// TestSessionsListCmdRun_LiveRunDriverIsRunningNotDone drives the real
// sessions scan List command, distinct from the classifier adapter above.
func TestSessionsListCmdRun_LiveRunDriverIsRunningNotDone(t *testing.T) {
	a, listCmd, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()
	parent, err := a.Sessions.CreateWithID(ctx, "list-run-driver-root", "root driven by a loop")
	require.NoError(t, err)
	cmdtest.ReleasedLock(t, dataDir, parent.ID)
	cmdtest.AddFinishedAssistant(t, a.Messages, parent.ID, message.FinishReasonEndTurn)

	driverHost := sessionhosts.NewHostStore(a.DB(), dataDir, os.Getpid(), "list-run-driver-fixture")
	driverHostID, err := driverHost.EnsureHost(ctx)
	require.NoError(t, err)
	require.NoError(t, driverHost.ClaimSessionDriver(ctx, parent.ID))
	driverHost.CloseKeepLock()
	t.Cleanup(func() { sessionhosts.ReleaseRetainedHostLocksForTest(driverHostID) })
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
	_, err = freshConn.ExecContext(ctx, `DELETE FROM session_drivers WHERE session_id = ?`, parent.ID)
	require.NoError(t, err)
	phase2 := rowFor(runList())
	require.NotContains(t, phase2, "running")
	require.NotContains(t, phase2, "done")
}
