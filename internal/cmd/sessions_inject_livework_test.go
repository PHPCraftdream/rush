package cmd

// R7C-3: `sessions inject` decided "running" from the session lock alone, so a
// live `rush run` loop between turns (lock released, driver marker live) was
// reported as "no process is currently running this session".

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// runInject runs the real `sessions inject` RunE against dataDir.
func runInject(t *testing.T, dataDir, sessionID string, interrupt, asJSON bool) (stdout, stderr string) {
	t.Helper()
	ensureRootFlagStandIns(sessionsInjectCmd, dataDir)
	if f := sessionsInjectCmd.Flags().Lookup("cwd"); f == nil {
		sessionsInjectCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsInjectCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsInjectCmd.Flags().Set("message", "also update the CHANGELOG"))
	require.NoError(t, sessionsInjectCmd.Flags().Set("file", ""))
	require.NoError(t, sessionsInjectCmd.Flags().Set("interrupt", boolFlag(interrupt)))
	require.NoError(t, sessionsInjectCmd.Flags().Set("json", boolFlag(asJSON)))
	sessionsInjectCmd.SetContext(context.Background())
	return captureStdoutAndStderr(t, func() {
		require.NoError(t, sessionsInjectCmd.RunE(sessionsInjectCmd, []string{sessionID}))
	})
}

// A loop driving the session between turns: released, aging lock, live marker.
func TestSessionsInjectCmdRun_LiveLoopBetweenTurnsIsRunning(t *testing.T) {
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()
	driven, err := a.Sessions.CreateWithID(ctx, "inject/driven x", "driven between turns")
	require.NoError(t, err)
	lone, err := a.Sessions.CreateWithID(ctx, "inject-lone", "no driver")
	require.NoError(t, err)
	ageLock(t, releasedLock(t, dataDir, driven.ID), 2*time.Minute)
	ageLock(t, releasedLock(t, dataDir, lone.ID), 2*time.Minute)
	store := a.AsyncJobStore()
	require.NotNil(t, store)
	require.NoError(t, store.ClaimSessionDriver(ctx, driven.ID))
	a.SetAsyncJobStoreForTest(nil)
	defer func() { _ = store.Close(context.Background()) }()
	a.Shutdown()

	decode := func(out string) injectResult {
		var res injectResult
		require.NoError(t, json.NewDecoder(strings.NewReader(out)).Decode(&res))
		return res
	}

	out, _ := runInject(t, dataDir, driven.ID, false, true)
	res := decode(out)
	require.True(t, res.Running, "a loop between turns runs the session")
	require.Equal(t, "injected", res.Status)
	require.True(t, res.BetweenTurns)
	require.Equal(t, os.Getpid(), res.DriverPID)

	out, _ = runInject(t, dataDir, driven.ID, true, true)
	res = decode(out)
	require.True(t, res.Running)
	require.Equal(t, "queued-for-interrupt", res.Status, "the queued row reads as the interrupt it is")
	require.True(t, res.BetweenTurns, "no turn is running: the flag says so")

	_, stderr := runInject(t, dataDir, driven.ID, true, false)
	require.Contains(t, stderr, "rush run PID "+strconv.Itoa(os.Getpid()), "the message names the loop")
	require.Contains(t, stderr, "between turns")
	require.NotContains(t, stderr, "no process is currently running")

	out, _ = runInject(t, dataDir, lone.ID, false, true)
	res = decode(out)
	require.False(t, res.Running, "no lock, no loop: unchanged")
	require.Equal(t, "persisted-offline", res.Status)
	require.False(t, res.BetweenTurns)
}

// R8C-4: a web session between turns with a running own job or a live
// delegation (no driver marker: only CLI loops claim one) is running, not
// "persisted-offline".
//
// Revert-check: reading only `.driver` from inspectSessionLiveWork makes the
// job and delegation rows report persisted-offline.
func TestSessionsInjectCmdRun_OwnJobOrDelegationBetweenTurnsIsRunning(t *testing.T) {
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()
	mk := func(id string) string {
		sess, err := a.Sessions.CreateWithID(ctx, id, id)
		require.NoError(t, err)
		ageLock(t, releasedLock(t, dataDir, sess.ID), 2*time.Minute)
		return sess.ID
	}
	jobID, delegID, loneID := mk("inject-web-job"), mk("inject-web-deleg"), mk("inject-web-lone")
	child, err := a.Sessions.CreateTaskSession(ctx, "inject-web-child", delegID, "sub-agent")
	require.NoError(t, err)
	store := a.AsyncJobStore()
	require.NotNil(t, store)
	claimOwnJob(t, store, jobID, "bash-1")
	claimDelegation(t, store, delegID, "delegate-1", child.ID)
	a.SetAsyncJobStoreForTest(nil)
	defer func() { _ = store.Close(context.Background()) }()
	a.Shutdown()

	decode := func(out string) injectResult {
		var res injectResult
		require.NoError(t, json.NewDecoder(strings.NewReader(out)).Decode(&res))
		return res
	}
	for _, id := range []string{jobID, delegID} {
		out, _ := runInject(t, dataDir, id, false, true)
		res := decode(out)
		require.True(t, res.Running, "%s: live work behind a released lock is running", id)
		require.Equal(t, "injected", res.Status, id)
		require.False(t, res.BetweenTurns, "%s: no CLI loop drives it", id)
		require.Zero(t, res.DriverPID, id)
	}

	_, stderr := runInject(t, dataDir, jobID, false, false)
	require.NotContains(t, stderr, "no process is currently running")
	require.Contains(t, stderr, "bash-1", "the message names what keeps the session open")

	out, _ := runInject(t, dataDir, loneID, false, true)
	res := decode(out)
	require.False(t, res.Running, "nothing live: unchanged")
	require.Equal(t, "persisted-offline", res.Status)
}
