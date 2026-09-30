package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSessionsReapCmdRun_HonorsConfiguredDataDir is the regression test for
// task #233 finding 2: sessionsReapCmdRun used to compute locksDir as
// filepath.Join(ResolveCwd(cmd), ".rush", "locks"), completely ignoring
// --data-dir / a configured data_directory. Unlike sessions kill/list/watch,
// this command is purely filesystem-based (no DB), so the fix uses the
// lightweight config.ResolveDataDirectory helper (task #224) exactly like
// sessions_kill.go does, rather than pulling in setupApp.
//
// This points --data-dir at a directory deliberately outside --cwd, seeds a
// lock file there for an orphaned (PID guaranteed dead) session, ages it past
// the 10s heartbeat threshold, and runs the real sessionsReapCmd.RunE. Before
// the fix this would print "(no locks directory)" (having looked in the
// wrong, cwd-based directory); after the fix it must find and remove the
// orphan lock at the configured location.
func TestSessionsReapCmdRun_HonorsConfiguredDataDir(t *testing.T) {
	// sessionsReapCmdRun now resolves the data directory via
	// config.ResolveDataDirectory, which — like config.Load/config.Init —
	// reads real config paths from the environment unless isolated. See
	// isolateConfigEnvForTests's doc comment (task #224 finding 3).
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	// Deliberately outside workDir entirely, so filepath.Join(cwd, ".rush")
	// can never accidentally coincide with this path.
	configuredDataDir := filepath.Join(tmp, "elsewhere-data")

	ensureRootFlagStandIns(sessionsReapCmd, configuredDataDir)
	if f := sessionsReapCmd.Flags().Lookup("cwd"); f == nil {
		sessionsReapCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsReapCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsReapCmd.Flags().Set("dry-run", "false"))
	require.NoError(t, sessionsReapCmd.Flags().Set("all", "false"))
	sessionsReapCmd.SetContext(context.Background())

	const sessionID = "configured-datadir-reap-id"
	lockDir := filepath.Join(configuredDataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	// PID 999999 is guaranteed not to be a live process on any platform.
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", 999999)), 0o644))
	// Age it past the 10s heartbeat threshold so reap treats it as eligible
	// rather than "skip-young".
	old := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(lockPath, old, old))

	// Sanity: the WRONG (pre-fix) path must not exist, so a false pass via
	// "(no locks directory)" being silently treated as success is impossible
	// to confuse with the real assertion below.
	wrongPath := filepath.Join(workDir, ".rush", "locks", "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	_, wrongStatErr := os.Stat(wrongPath)
	require.True(t, os.IsNotExist(wrongStatErr))

	stderr := captureStderr(t, func() {
		runErr := sessionsReapCmd.RunE(sessionsReapCmd, nil)
		require.NoError(t, runErr)
	})
	t.Logf("sessions reap stderr:\n%s", stderr)

	require.NotContains(t, stderr, "(no locks directory)",
		"fix must find the locks dir at the --data-dir-configured location, not report it missing")
	require.Contains(t, stderr, "reclaimed 1 lock",
		"fix must reap the orphan lock seeded at the configured data dir")

	_, statErr := os.Stat(lockPath)
	require.True(t, os.IsNotExist(statErr), "orphan lock file at the configured data dir must be removed")
}

// R8C-5: an aged EMPTY lock file is the leftover of a clean release (a live
// `rush run` loop between turns, a web session between turns). Probing it
// takes the lock the owner needs for its next turn and unlinks a path it will
// reuse, so reap keeps it and reports "removed orphan ... provably dead" for
// nothing that died. A lock recording a PID (a crash leaves one) is still
// probed and reclaimed.
//
// Revert-check: dropping the PID-less skip makes the released lock read
// "would remove orphan lock" (and, without --dry-run, unlinks it).
func TestSessionsReap_ReleasedLockIsKeptCrashLockIsReaped(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)
	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	dataDir := filepath.Join(tmp, "reap-released-data")
	ensureRootFlagStandIns(sessionsReapCmd, dataDir)
	if f := sessionsReapCmd.Flags().Lookup("cwd"); f == nil {
		sessionsReapCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsReapCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsReapCmd.Flags().Set("all", "false"))
	sessionsReapCmd.SetContext(context.Background())

	releasedLockPath := releasedLock(t, dataDir, "reap-released-loop")
	ageLock(t, releasedLockPath, 2*time.Minute)
	crashPath := writeLockFileAt(t, dataDir, "reap-crashed", 999999)
	ageLock(t, crashPath, 2*time.Minute)
	before, err := os.Stat(releasedLockPath)
	require.NoError(t, err)

	require.NoError(t, sessionsReapCmd.Flags().Set("dry-run", "true"))
	stderr := captureStderr(t, func() { require.NoError(t, sessionsReapCmd.RunE(sessionsReapCmd, nil)) })
	require.Contains(t, stderr, "would remove orphan lock session-reap-crashed.lock")
	require.NotContains(t, stderr, "session-reap-released-loop.lock (holder provably dead",
		"a clean-release leftover is not an orphan")
	require.Contains(t, stderr, "kept   released lock session-reap-released-loop.lock")
	require.Contains(t, stderr, "would have reclaimed 1 lock(s)")

	require.NoError(t, sessionsReapCmd.Flags().Set("dry-run", "false"))
	// The dry run still probed the crash lock (freshening it): seed it again.
	ageLock(t, writeLockFileAt(t, dataDir, "reap-crashed", 999999), 2*time.Minute)
	stderr = captureStderr(t, func() { require.NoError(t, sessionsReapCmd.RunE(sessionsReapCmd, nil)) })
	require.Contains(t, stderr, "reclaimed 1 lock(s)")
	require.FileExists(t, releasedLockPath, "the loop's lock file stays in place")
	after, err := os.Stat(releasedLockPath)
	require.NoError(t, err)
	require.True(t, before.ModTime().Equal(after.ModTime()), "not probed: the probe freshens the mtime")
	_, err = os.Stat(crashPath)
	require.True(t, os.IsNotExist(err), "the crash-orphan is reclaimed as before")
}
