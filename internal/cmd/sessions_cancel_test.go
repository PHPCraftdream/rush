package cmd

// R8A-2 / R8C-2 (cmd part): `sessions cancel --all` used to flag every
// top-level session, idle web ones included, and the flag then aborted their
// next turn after one step. --all now flags only sessions with live work.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func runCancel(t *testing.T, dataDir string, all bool, args ...string) (stderr string, err error) {
	t.Helper()
	ensureRootFlagStandIns(sessionsCancelCmd, dataDir)
	if f := sessionsCancelCmd.Flags().Lookup("cwd"); f == nil {
		sessionsCancelCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsCancelCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsCancelCmd.Flags().Set("all", boolFlag(all)))
	sessionsCancelCmd.SetContext(context.Background())
	stderr = captureStderr(t, func() { err = sessionsCancelCmd.RunE(sessionsCancelCmd, args) })
	return stderr, err
}

// Revert-check: flagging every listed session again (dropping the live-work
// filter) fails the idle rows; skipping the lock / job / delegation / driver
// signal fails that row.
func TestSessionsCancelAll_FlagsOnlySessionsWithLiveWork(t *testing.T) {
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	ctx := context.Background()
	mk := func(id string) string {
		sess, err := a.Sessions.CreateWithID(ctx, id, id)
		require.NoError(t, err)
		return sess.ID
	}
	idle, released := mk("cancel-idle"), mk("cancel-released")
	driven, owner, parent, locked := mk("cancel-driven"), mk("cancel-owner"), mk("cancel-parent"), mk("cancel-locked")
	ageLock(t, releasedLock(t, dataDir, released), 2*time.Minute)
	child, err := a.Sessions.CreateTaskSession(ctx, "cancel-child", parent, "sub-agent")
	require.NoError(t, err)
	writeLockFileAt(t, dataDir, locked, os.Getpid())

	store := a.AsyncJobStore()
	require.NotNil(t, store)
	require.NoError(t, store.ClaimSessionDriver(ctx, driven))
	claimOwnJob(t, store, owner, "bash-1")
	claimDelegation(t, store, parent, "delegate-1", child.ID)
	a.SetAsyncJobStoreForTest(nil)
	defer func() { _ = store.Close(context.Background()) }()
	a.Shutdown()

	stderr, err := runCancel(t, dataDir, true)
	require.NoError(t, err)
	require.Contains(t, stderr, "cancellation requested for 4 session(s)")
	require.Contains(t, stderr, "skipped 2 session(s) with no live work")

	conn, err := dbConnectForTest(t, dataDir)
	require.NoError(t, err)
	flagged := func(id string) bool {
		var v int
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT cancel_requested FROM sessions WHERE id = ?`, id).Scan(&v))
		return v != 0
	}
	for _, id := range []string{driven, owner, parent, locked} {
		require.True(t, flagged(id), "%s has live work: flagged", id)
	}
	for _, id := range []string{idle, released} {
		require.False(t, flagged(id), "%s is idle: not flagged", id)
	}

	// A single id keeps flagging: the flag is a one-shot request the operator
	// asked for by name.
	stderr, err = runCancel(t, dataDir, false, idle)
	require.NoError(t, err)
	require.Contains(t, stderr, "cancellation requested for session "+idle)
	require.True(t, flagged(idle))
}

// boolFlag renders a bool for Flags().Set, the shape the deleted
// sessions_livework_test.go used to provide.
func boolFlag(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
