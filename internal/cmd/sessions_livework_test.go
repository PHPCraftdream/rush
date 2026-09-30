package cmd

// R6C-1 / R6C-2: in phase 4 the session lock is held only during a turn and
// its file is truncated (not deleted) on release, so a live `rush run` loop
// waiting between turns leaves an EMPTY, aging lock file, an ended_reason
// from the last turn and a finished last assistant message. The durable
// facts that it is still working are the driver marker and the running
// job/delegation rows. watch / tail / locks / list / why must read them
// instead of treating "no live lock" as "the run ended" or "crashed".

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// releasedLock writes what a clean release leaves behind: an empty lock file
// (no PID) whose mtime is older than the heartbeat window.
func releasedLock(t *testing.T, dataDir, sessionID string) string {
	t.Helper()
	path := writeLockFileAt(t, dataDir, sessionID, 0)
	require.NoError(t, os.WriteFile(path, nil, 0o644))
	backDateLock(t, path)
	return path
}

// betweenTurnsSession seeds the realistic first-turn-ended shape: ended_reason
// set by the turn, an empty back-dated lock, the last assistant message
// finished with finish.
func betweenTurnsSession(t *testing.T, s session.Service, m message.Service, dataDir, title string, finish message.FinishReason) session.Session {
	t.Helper()
	ctx := context.Background()
	sess, err := s.Create(ctx, title)
	require.NoError(t, err)
	addFinishedAssistant(t, m, sess.ID, finish)
	require.NoError(t, s.SetEndedReason(ctx, sess.ID, string(finish)))
	releasedLock(t, dataDir, sess.ID)
	return sess
}

// R6C-1: `sessions watch` must not print "session ended" while a live loop
// waits between turns; releasing the marker (the loop exited) ends the watch.
//
// Revert-check: dropping the live-work consultation from isSessionFinished
// makes the first assertion fail (done is true, the verdict is watchExit).
func TestIsSessionFinished_LiveRunDriverBetweenTurnsKeepsWatching(t *testing.T) {
	t.Parallel()
	a, s, m, store, dataDir := newRunDriverTestApp(t)
	ctx := context.Background()
	sess := betweenTurnsSession(t, s, m, dataDir, "loop waiting on a job", message.FinishReasonEndTurn)
	require.NoError(t, store.ClaimSessionDriver(ctx, sess.ID))

	st, reason := isSessionFinished(ctx, a, sess.ID, dataDir)
	require.False(t, st.done, "a live loop between turns is not a finished session (reason %q)", reason)
	require.Equal(t, watchKeepWatching, decideWatchExit(st, false, time.Hour, time.Hour),
		"even a long-quiet watch must keep watching while the driver is live")
	require.Contains(t, st.liveWork, "PID 4321", "the wait note names the loop")

	require.NoError(t, store.ReleaseSessionDriver(ctx, sess.ID))
	st, reason = isSessionFinished(ctx, a, sess.ID, dataDir)
	require.True(t, st.done, "the loop released its marker: the ended state applies again")
	require.Equal(t, "end_turn", reason)
	require.Equal(t, watchExit, decideWatchExit(st, false, time.Hour, time.Hour))
}

// The same shape, waiting on the session's own running job or on a
// delegation instead of a driver marker.
//
// Revert-check: as above, per row.
func TestIsSessionFinished_OwnJobAndDelegationBetweenTurnsKeepWatching(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		hold func(t *testing.T, s session.Service, store *session.AsyncJobStore, sessionID string)
		note string
	}{
		{"own job", func(t *testing.T, _ session.Service, store *session.AsyncJobStore, id string) {
			claimOwnJob(t, store, id, "bash-1")
		}, "bash-1"},
		{"delegation", func(t *testing.T, s session.Service, store *session.AsyncJobStore, id string) {
			child, err := s.CreateTaskSession(ctx, "watch-child-"+id, id, "sub-agent")
			require.NoError(t, err)
			claimDelegation(t, store, id, "delegate-1", child.ID)
		}, "descendant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, s, m, store, dataDir := newRunDriverTestApp(t)
			sess := betweenTurnsSession(t, s, m, dataDir, "waiting: "+tc.name, message.FinishReasonEndTurn)
			tc.hold(t, s, store, sess.ID)

			st, reason := isSessionFinished(ctx, a, sess.ID, dataDir)
			require.False(t, st.done, "must keep watching (reason %q)", reason)
			require.Contains(t, st.liveWork, tc.note)
		})
	}
}

// A marker on a provably dead host (the loop crashed without releasing it)
// keeps nothing open: the watch ends.
func TestIsSessionFinished_DeadHostDriverEndsTheWatch(t *testing.T) {
	t.Parallel()
	a, s, m, _, dataDir := newRunDriverTestApp(t)
	ctx := context.Background()
	sess := betweenTurnsSession(t, s, m, dataDir, "driver crashed", message.FinishReasonEndTurn)
	dead := newDeadHostStore(t, dataDir, 4242, "dead-loop")
	require.NoError(t, dead.ClaimSessionDriver(ctx, sess.ID))
	require.NoError(t, dead.SimulateCrashForTest())

	st, _ := isSessionFinished(ctx, a, sess.ID, dataDir)
	require.True(t, st.done)
	require.Empty(t, st.liveWork)
}

// R6C-1 (tail): `sessions tail --follow` stops on the newest message's finish;
// with a live driver / running job that finish is only the first turn's.
//
// Revert-check: dropping the consultation from tailSessionFinished makes the
// driver phase return true.
func TestTailSessionFinished_LiveWorkKeepsFollowing(t *testing.T) {
	t.Parallel()
	a, s, m, store, dataDir := newRunDriverTestApp(t)
	ctx := context.Background()
	sess := betweenTurnsSession(t, s, m, dataDir, "tail between turns", message.FinishReasonEndTurn)
	require.True(t, tailDone(ctx, a, sess.ID), "no live work: the finish ends the follow")

	require.NoError(t, store.ClaimSessionDriver(ctx, sess.ID))
	done, note := tailSessionFinished(ctx, a, sess.ID)
	require.False(t, done, "a live loop between turns keeps the follow going")
	require.Contains(t, note, "`rush run`", "the follow names what it waits on")
	require.NoError(t, store.ReleaseSessionDriver(ctx, sess.ID))
	require.True(t, tailDone(ctx, a, sess.ID))

	claimOwnJob(t, store, sess.ID, "bash-1")
	require.False(t, tailDone(ctx, a, sess.ID), "a running own job keeps the follow going")
}

// R6C-2: the shape a failed reaction turn leaves -- an EMPTY back-dated lock,
// an error finish, a live driver marker, pending debt -- is running (the loop
// retries in 60 s), not crashed; a lock recording a dead PID that is not the
// driver's stays a crash; no live work stays a crash.
//
// Revert-check: dropping the clean-release rule from explainSessionStatus
// makes the first row read "status: crashed".
func TestExplainSessionStatus_ReleasedLockErrorFinishLiveDriverIsRunning(t *testing.T) {
	t.Parallel()
	a, s, m, store, dataDir := newRunDriverTestApp(t)
	ctx := context.Background()

	driven := betweenTurnsSession(t, s, m, dataDir, "failed drain, paced retry", message.FinishReasonError)
	require.NoError(t, store.InsertSessionNotice(ctx, driven.ID, "job_result", "a job finished", true, ""))
	require.NoError(t, store.ClaimSessionDriver(ctx, driven.ID))

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, driven.ID, &buf))
	out := buf.String()
	require.Equal(t, "status: running (stale lock)", verdictLine(out))
	require.Contains(t, out, "`rush run`")
	require.Contains(t, out, "reaction is owed")
	require.NotContains(t, out, "likely died mid-turn")
	require.NotContains(t, out, "grep rush.log")

	// Another process's dead PID in the lock is a genuine crash of that
	// process, even beside a live driver.
	other := betweenTurnsSession(t, s, m, dataDir, "other crashed, driven", message.FinishReasonError)
	backDateLock(t, writeLockFileAt(t, dataDir, other.ID, 999999))
	require.NoError(t, store.ClaimSessionDriver(ctx, other.ID))
	buf.Reset()
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, other.ID, &buf))
	require.Equal(t, "status: crashed", verdictLine(buf.String()))

	// No live work at all: the empty lock plus an error finish is still a
	// failed session, reported as before.
	lone := betweenTurnsSession(t, s, m, dataDir, "failed, nothing live", message.FinishReasonError)
	buf.Reset()
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, lone.ID, &buf))
	require.Equal(t, "status: crashed", verdictLine(buf.String()))
}

// The clean-release rule holds for the other two live-work signals.
//
// Revert-check: as above, per row.
func TestExplainSessionStatus_ReleasedLockErrorFinishOwnJobAndDelegation(t *testing.T) {
	t.Parallel()
	a, s, m, store, dataDir := newRunDriverTestApp(t)
	ctx := context.Background()

	own := betweenTurnsSession(t, s, m, dataDir, "error finish, own job", message.FinishReasonError)
	claimOwnJob(t, store, own.ID, "bash-1")
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, own.ID, &buf))
	require.Equal(t, "status: running (stale lock)", verdictLine(buf.String()))
	require.Contains(t, buf.String(), "its own background job bash-1")

	parent := betweenTurnsSession(t, s, m, dataDir, "error finish, delegation", message.FinishReasonError)
	child, err := s.CreateTaskSession(ctx, "livework-child", parent.ID, "sub-agent")
	require.NoError(t, err)
	claimDelegation(t, store, parent.ID, "delegate-1", child.ID)
	buf.Reset()
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, parent.ID, &buf))
	require.Equal(t, "status: delegating (stale lock)", verdictLine(buf.String()))
}

// promoteCleanReleaseCrashes (the list half of R6C-2): only a "crashed"
// session whose lock is a clean release AND that has live work is promoted.
//
// Revert-check: making the function return statusByID untouched fails the
// promotion rows.
func TestPromoteCleanReleaseCrashes(t *testing.T) {
	t.Parallel()
	a, s, m, store, dataDir := newRunDriverTestApp(t)
	ctx := context.Background()
	mk := func(title string, finish message.FinishReason) session.Session {
		return betweenTurnsSession(t, s, m, dataDir, title, finish)
	}
	driven, owned, delegating, deadPID, idle := mk("driven", message.FinishReasonError), mk("owned", message.FinishReasonError),
		mk("delegating", message.FinishReasonError), mk("dead-pid", message.FinishReasonError), mk("idle", message.FinishReasonError)
	require.NoError(t, store.ClaimSessionDriver(ctx, driven.ID))
	claimOwnJob(t, store, owned.ID, "bash-1")
	child, err := s.CreateTaskSession(ctx, "promote-child", delegating.ID, "sub-agent")
	require.NoError(t, err)
	claimDelegation(t, store, delegating.ID, "delegate-1", child.ID)
	backDateLock(t, writeLockFileAt(t, dataDir, deadPID.ID, 999999))
	require.NoError(t, store.ClaimSessionDriver(ctx, deadPID.ID))

	sessions := []session.Session{driven, owned, delegating, deadPID, idle}
	got := promoteCleanReleaseCrashes(ctx, a, dataDir, sessions, map[string]string{
		driven.ID: "crashed", owned.ID: "crashed", delegating.ID: "crashed", deadPID.ID: "crashed", idle.ID: "crashed",
	})
	require.Equal(t, "running", got[driven.ID])
	require.Equal(t, "running", got[owned.ID])
	require.Equal(t, "delegating", got[delegating.ID])
	require.Equal(t, "crashed", got[deadPID.ID], "a recorded dead PID that is not the driver's is a crash")
	require.Equal(t, "crashed", got[idle.ID], "no live work: still crashed")

	require.Nil(t, promoteCleanReleaseCrashes(ctx, nil, dataDir, sessions, nil), "nil app is a no-op")
}

// R6C-2 through the real `sessions list`: the failed-drain shape reads
// running, and crashed again once the loop released its marker.
//
// Revert-check: dropping the promoteCleanReleaseCrashes call from the list
// command fails phase 1 (the row reads "crashed").
func TestSessionsListCmdRun_ReleasedLockErrorFinishLiveDriverIsRunning(t *testing.T) {
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()

	root, err := a.Sessions.CreateWithID(ctx, "list-failed-drain-root", "root, failed reaction turn")
	require.NoError(t, err)
	addFinishedAssistant(t, a.Messages, root.ID, message.FinishReasonError)
	releasedLock(t, dataDir, root.ID)

	store := a.AsyncJobStore()
	require.NotNil(t, store)
	require.NoError(t, store.ClaimSessionDriver(ctx, root.ID))
	a.SetAsyncJobStoreForTest(nil)
	defer func() { _ = store.Close(context.Background()) }()
	a.Shutdown()

	runList := func() string {
		out := captureStdout(t, func() { require.NoError(t, sessionsListCmd.RunE(sessionsListCmd, nil)) })
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, root.ID[:8]) {
				return line
			}
		}
		return ""
	}
	phase1 := runList()
	require.NotEmpty(t, phase1)
	require.Contains(t, phase1, "running", "a paced retry of a live loop is running, not crashed")
	require.NotContains(t, phase1, "crashed")

	freshConn, err := dbConnectForTest(t, dataDir)
	require.NoError(t, err)
	_, err = freshConn.ExecContext(ctx, `DELETE FROM session_drivers WHERE session_id = ?`, root.ID)
	require.NoError(t, err)
	require.Contains(t, runList(), "crashed", "the loop is gone: the failed session reads as before")
}

// R6C-1 (locks): a released lock of a session a live loop drives is "between
// turns", not offline; without the marker it is offline; --stale-only does not
// list it and --prune leaves its lock file alone.
//
// The driven id contains characters the lock file name replaces ("/", space):
// the marker is keyed by the real id, the file by the sanitised one (R7C-2).
//
// Revert-check: dropping the driver consultation from `sessions locks`
// makes the driven row read "offline" / stale; looking the marker up by the
// file-derived name (the pre-R7C-2 key) does the same for this id.
func TestSessionsLocksCmdRun_LiveDriverBetweenTurnsIsNotOffline(t *testing.T) {
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()

	driven, err := a.Sessions.CreateWithID(ctx, "locks/driven x", "driven between turns")
	require.NoError(t, err)
	lone, err := a.Sessions.CreateWithID(ctx, "locks-lone", "no driver")
	require.NoError(t, err)
	drivenLock := releasedLock(t, dataDir, driven.ID)
	loneLock := releasedLock(t, dataDir, lone.ID)
	ageLock(t, drivenLock, 2*time.Minute)
	ageLock(t, loneLock, 2*time.Minute)

	store := a.AsyncJobStore()
	require.NotNil(t, store)
	require.NoError(t, store.ClaimSessionDriver(ctx, driven.ID))
	a.SetAsyncJobStoreForTest(nil)
	defer func() { _ = store.Close(context.Background()) }()
	a.Shutdown()

	ensureRootFlagStandIns(sessionsLocksCmd, dataDir)
	if f := sessionsLocksCmd.Flags().Lookup("cwd"); f == nil {
		sessionsLocksCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsLocksCmd.Flags().Set("cwd", ""))
	sessionsLocksCmd.SetContext(ctx)

	type item struct {
		SessionID    string `json:"session_id"`
		PID          int    `json:"pid"`
		Pulse        string `json:"pulse"`
		Stale        bool   `json:"stale"`
		BetweenTurns bool   `json:"between_turns"`
		DriverPID    int    `json:"driver_pid"`
	}
	runLocks := func(stale, prune bool) map[string]item {
		require.NoError(t, sessionsLocksCmd.Flags().Set("json", "true"))
		require.NoError(t, sessionsLocksCmd.Flags().Set("stale-only", boolFlag(stale)))
		require.NoError(t, sessionsLocksCmd.Flags().Set("prune", boolFlag(prune)))
		stdout, _ := captureStdoutAndStderr(t, func() { require.NoError(t, sessionsLocksCmd.RunE(sessionsLocksCmd, nil)) })
		got := map[string]item{}
		dec := json.NewDecoder(strings.NewReader(stdout))
		for dec.More() {
			var it item
			require.NoError(t, dec.Decode(&it))
			got[it.SessionID] = it
		}
		return got
	}

	drivenRow := sanitiseSessionIDForFilename(driven.ID)
	all := runLocks(false, false)
	require.Contains(t, all, drivenRow)
	require.NotEqual(t, "offline", all[drivenRow].Pulse)
	require.False(t, all[drivenRow].Stale)
	require.True(t, all[drivenRow].BetweenTurns)
	require.Equal(t, os.Getpid(), all[drivenRow].DriverPID)
	require.Equal(t, "offline", all[lone.ID].Pulse, "no driver: an aged released lock is offline as before")
	require.False(t, all[lone.ID].BetweenTurns)

	staleOnly := runLocks(true, false)
	require.NotContains(t, staleOnly, drivenRow)
	require.Contains(t, staleOnly, lone.ID)

	runLocks(false, true)
	_, err = os.Stat(drivenLock)
	require.NoError(t, err, "--prune never touches the lock of a session a live loop drives")
	_, err = os.Stat(loneLock)
	require.True(t, os.IsNotExist(err), "the undriven provably-dead lock is pruned as before")
}

// The table form names the loop.
func TestSessionsLocksCmdRun_TableNamesTheBetweenTurnsLoop(t *testing.T) {
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()
	driven, err := a.Sessions.CreateWithID(ctx, "locks-table-driven", "driven between turns")
	require.NoError(t, err)
	releasedLock(t, dataDir, driven.ID)
	store := a.AsyncJobStore()
	require.NotNil(t, store)
	require.NoError(t, store.ClaimSessionDriver(ctx, driven.ID))
	a.SetAsyncJobStoreForTest(nil)
	defer func() { _ = store.Close(context.Background()) }()
	a.Shutdown()

	ensureRootFlagStandIns(sessionsLocksCmd, dataDir)
	if f := sessionsLocksCmd.Flags().Lookup("cwd"); f == nil {
		sessionsLocksCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsLocksCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsLocksCmd.Flags().Set("json", "false"))
	require.NoError(t, sessionsLocksCmd.Flags().Set("stale-only", "false"))
	require.NoError(t, sessionsLocksCmd.Flags().Set("prune", "false"))
	sessionsLocksCmd.SetContext(ctx)

	stdout, _ := captureStdoutAndStderr(t, func() { require.NoError(t, sessionsLocksCmd.RunE(sessionsLocksCmd, nil)) })
	require.Contains(t, stdout, fmt.Sprintf("between turns (rush run PID %d)", os.Getpid()))
	require.NotContains(t, stdout, "offline")
}

func boolFlag(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// newDeadHostStore opens a second store on the data dir (a distinct host).
func newDeadHostStore(t *testing.T, dataDir string, pid int, label string) *session.AsyncJobStore {
	t.Helper()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	return session.NewAsyncJobStore(conn, dataDir, pid, label)
}

func dbConnectForTest(t *testing.T, dataDir string) (*sql.DB, error) {
	t.Helper()
	conn, err := db.Connect(context.Background(), dataDir)
	if err == nil {
		t.Cleanup(func() { _ = db.Release(dataDir) })
	}
	return conn, err
}

// ageLock back-dates a lock file's mtime by d.
func ageLock(t *testing.T, path string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	require.NoError(t, os.Chtimes(path, old, old))
}

func tailDone(ctx context.Context, a *app.App, sessionID string) bool {
	done, _ := tailSessionFinished(ctx, a, sessionID)
	return done
}

// R7C-2: ids that sanitise to the same lock-file stem ("x/y" and "x y") share
// one file; only a loop's own (or no) recorded PID reads as between turns, so
// a dead colliding session's PID never makes its lock look live, and the
// candidate order is deterministic.
//
// Revert-check: making lockDriver ignore the recorded PID reads the dead PID
// as "between turns"; dropping the PID sort makes the candidate order depend
// on map iteration.
func TestLockDriver_CollidingIdsShareOneStem(t *testing.T) {
	drivers := map[string]session.SessionDriver{
		"x/y": {SessionID: "x/y", PID: 300},
		"x y": {SessionID: "x y", PID: 200},
		"z":   {SessionID: "z", PID: 100},
	}
	for range 20 {
		byName := driversByLockName(drivers)
		require.Len(t, byName, 2)
		require.Equal(t, []int64{200, 300}, []int64{byName["x_y"][0].PID, byName["x_y"][1].PID})
		require.Len(t, byName["z"], 1)
	}
	byName := driversByLockName(drivers)

	d, ok := lockDriver(byName["x_y"], 0)
	require.True(t, ok)
	require.EqualValues(t, 200, d.PID, "an empty file belongs to the lowest-PID candidate")
	d, ok = lockDriver(byName["x_y"], 300)
	require.True(t, ok)
	require.EqualValues(t, 300, d.PID, "a recorded PID selects its own loop")
	_, ok = lockDriver(byName["x_y"], 999999)
	require.False(t, ok, "a foreign recorded PID (a dead colliding session) is not a live loop's")
	_, ok = lockDriver(nil, 0)
	require.False(t, ok)
}

// R7C-2 (locks, command level): the colliding dead session's recorded PID
// keeps the row offline even though a live loop drives the other id.
//
// Revert-check: matching on the marker without the PID condition reads the
// row as between turns.
func TestSessionsLocksCmdRun_CollidingDeadSessionStaysOffline(t *testing.T) {
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()
	driven, err := a.Sessions.CreateWithID(ctx, "clash/id", "driven")
	require.NoError(t, err)
	path := writeLockFileAt(t, dataDir, "clash id", 999999)
	ageLock(t, path, 2*time.Minute)
	store := a.AsyncJobStore()
	require.NotNil(t, store)
	require.NoError(t, store.ClaimSessionDriver(ctx, driven.ID))
	a.SetAsyncJobStoreForTest(nil)
	defer func() { _ = store.Close(context.Background()) }()
	a.Shutdown()

	ensureRootFlagStandIns(sessionsLocksCmd, dataDir)
	if f := sessionsLocksCmd.Flags().Lookup("cwd"); f == nil {
		sessionsLocksCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsLocksCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsLocksCmd.Flags().Set("json", "true"))
	require.NoError(t, sessionsLocksCmd.Flags().Set("stale-only", "false"))
	require.NoError(t, sessionsLocksCmd.Flags().Set("prune", "false"))
	sessionsLocksCmd.SetContext(ctx)

	stdout, _ := captureStdoutAndStderr(t, func() { require.NoError(t, sessionsLocksCmd.RunE(sessionsLocksCmd, nil)) })
	var row struct {
		SessionID    string `json:"session_id"`
		Pulse        string `json:"pulse"`
		Stale        bool   `json:"stale"`
		BetweenTurns bool   `json:"between_turns"`
	}
	require.NoError(t, json.NewDecoder(strings.NewReader(stdout)).Decode(&row))
	require.Equal(t, "clash_id", row.SessionID)
	require.Equal(t, "offline", row.Pulse)
	require.True(t, row.Stale)
	require.False(t, row.BetweenTurns)
}

// With Sessions.Get carrying ended_reason (R7C-4), the watch's signal (a) is
// live: a row that says "canceled" ends the watch with that reason once the
// loop is gone, but never while the driver marker is live (the live-work
// consultation runs before the verdict is trusted).
//
// Revert-check: dropping the live-work consultation from isSessionFinished
// makes the first assertion fail; dropping ended_reason from Get's result
// changes the second reason to "end_turn".
func TestIsSessionFinished_EndedReasonEndsTheWatchOnlyWhenNoLoopIsLive(t *testing.T) {
	t.Parallel()
	a, s, m, store, dataDir := newRunDriverTestApp(t)
	ctx := context.Background()
	sess := betweenTurnsSession(t, s, m, dataDir, "canceled row, live loop", message.FinishReasonEndTurn)
	require.NoError(t, s.SetEndedReason(ctx, sess.ID, "canceled"))
	require.NoError(t, store.ClaimSessionDriver(ctx, sess.ID))

	st, reason := isSessionFinished(ctx, a, sess.ID, dataDir)
	require.False(t, st.done, "a live loop keeps the watch going whatever the row says (reason %q)", reason)

	require.NoError(t, store.ReleaseSessionDriver(ctx, sess.ID))
	st, reason = isSessionFinished(ctx, a, sess.ID, dataDir)
	require.True(t, st.done)
	require.Equal(t, "canceled", reason, "signal (a): the row's ended_reason is the reason")
}
