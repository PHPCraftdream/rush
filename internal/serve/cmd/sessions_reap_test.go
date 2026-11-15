package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
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

func TestAcquireUnixReapProbePreservesHolderMetadata(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	const sessionID = "reap-probe-holder-metadata"
	path := filepath.Join(dataDir, "locks", "session-"+sessionID+".lock")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("placeholder\n"), 0o644))

	// Exercise the exact Unix production acquisition policy, including on
	// Windows, where the Unix unlink/release ordering cannot otherwise run.
	probe, err := acquireUnixReapProbe(dataDir, sessionID)
	require.NoError(t, err)
	require.NoError(t, probe.Release())

	// Release performs metadata cleanup asynchronously. Wait for its normal
	// handoff window before checking that the production no-cleanup option
	// preserved the acquired lock's metadata. This also works on Windows,
	// where the OS lock prevents reading the file while the probe is held.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(path + ".gen"); statErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%d\n", os.Getpid()), string(got))
	pidAfter, err := os.ReadFile(path + ".pid")
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%d\n", os.Getpid()), string(pidAfter))
	_, err = os.Stat(path + ".gen")
	require.NoError(t, err)
}

func TestSessionsReap_ReleaseAfterUnlinkDoesNotTouchReplacement(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)
	dataDir := filepath.Join(tmp, "reap-release-replacement")
	ensureRootFlagStandIns(sessionsReapCmd, dataDir)
	if f := sessionsReapCmd.Flags().Lookup("cwd"); f == nil {
		sessionsReapCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsReapCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsReapCmd.Flags().Set("dry-run", "false"))
	require.NoError(t, sessionsReapCmd.Flags().Set("all", "false"))
	sessionsReapCmd.SetContext(context.Background())
	path := writeLockFileAt(t, dataDir, "reap-release-replacement", 999999)
	ageLock(t, path, time.Minute)

	oldHook := reapAfterUnlinkHook
	var replacement *session.SessionLock
	reapAfterUnlinkHook = func(string) {
		var err error
		replacement, err = session.TryAcquireSessionLock(dataDir, "reap-release-replacement")
		require.NoError(t, err, "new owner can acquire recreated path after unlink")
	}
	t.Cleanup(func() { reapAfterUnlinkHook = oldHook })
	stderr := captureStderr(t, func() { require.NoError(t, sessionsReapCmd.RunE(sessionsReapCmd, nil)) })
	require.NotNil(t, replacement)
	t.Cleanup(func() { _ = replacement.Release() })
	require.FileExists(t, path)
	_, err := session.TryAcquireSessionLock(dataDir, "reap-release-replacement")
	var busyErr *session.SessionLockBusyError
	require.ErrorAs(t, err, &busyErr, "replacement's OS lock remains held")
	replacementPID, err := os.ReadFile(path + ".pid")
	require.NoError(t, err, "replacement PID sidecar survives stale probe cleanup")
	require.Equal(t, fmt.Sprintf("%d\n", os.Getpid()), string(replacementPID))
	require.FileExists(t, path+".gen", "replacement generation sidecar survives stale probe cleanup")
	require.Contains(t, stderr, "reclaimed 1 lock(s)")
}

func TestSessionsReap_UnlinkWhileProbeLockHeld(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)
	dataDir := filepath.Join(tmp, "reap-unlink-held")
	ensureRootFlagStandIns(sessionsReapCmd, dataDir)
	if f := sessionsReapCmd.Flags().Lookup("cwd"); f == nil {
		sessionsReapCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsReapCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsReapCmd.Flags().Set("dry-run", "false"))
	require.NoError(t, sessionsReapCmd.Flags().Set("all", "false"))
	sessionsReapCmd.SetContext(context.Background())
	path := writeLockFileAt(t, dataDir, "reap-unlink-race", 999999)
	ageLock(t, path, time.Minute)

	var owner *session.SessionLock
	oldHook := reapAfterProbeHook
	hookCalls := 0
	reapAfterProbeHook = func(string) {
		hookCalls++
		if hookCalls != 1 {
			return
		}
		var err error
		if runtime.GOOS == "windows" {
			deadline := time.Now().Add(10 * time.Second)
			for {
				owner, err = session.TryAcquireSessionLock(dataDir, "reap-unlink-race")
				if err == nil || time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			require.NoError(t, err, "owner must acquire after probe release and before fresh probe")
		} else {
			owner, err = session.TryAcquireSessionLock(dataDir, "reap-unlink-race")
			var busyErr *session.SessionLockBusyError
			require.ErrorAs(t, err, &busyErr, "the probe's held flock must prevent a competing owner")
		}
	}
	t.Cleanup(func() { reapAfterProbeHook = oldHook })

	stderr := captureStderr(t, func() { require.NoError(t, sessionsReapCmd.RunE(sessionsReapCmd, nil)) })
	require.Equal(t, 1, hookCalls, "probe hook must run exactly once")
	if runtime.GOOS == "windows" {
		require.NotNil(t, owner, "owner acquired in release-to-fresh-probe window must survive")
		t.Cleanup(func() { _ = owner.Release() })
		require.Contains(t, stderr, "kept   live lock")
		require.FileExists(t, path, "live owner's lock path must not be removed")
	} else {
		require.Nil(t, owner)
		require.Contains(t, stderr, "reclaimed 1 lock(s)")
		_, err := os.Stat(path)
		require.True(t, os.IsNotExist(err), "uncontended stale lock is removed after safe unlink")
	}
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
	held, err := session.TryAcquireSessionLock(dataDir, "reap-held")
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Release() })
	heldPath := filepath.Join(dataDir, "locks", "session-reap-held.lock")
	ageLock(t, heldPath, 2*time.Minute)
	heldBefore, err := os.Stat(heldPath)
	require.NoError(t, err)
	before, err := os.Stat(releasedLockPath)
	require.NoError(t, err)

	crashBefore, err := os.Stat(crashPath)
	require.NoError(t, err)
	beforePID := session.ReadLockPID(crashPath)

	require.NoError(t, sessionsReapCmd.Flags().Set("dry-run", "true"))
	stderr := captureStderr(t, func() { require.NoError(t, sessionsReapCmd.RunE(sessionsReapCmd, nil)) })
	require.Contains(t, stderr, "would probe lock session-reap-crashed.lock (aged PID recorded; candidate")
	require.Contains(t, stderr, "would probe lock session-reap-held.lock (aged PID recorded; candidate")
	require.NotContains(t, stderr, "session-reap-crashed.lock (holder provably dead",
		"dry-run has not probed the lock")
	require.NotContains(t, stderr, "session-reap-released-loop.lock (holder provably dead",
		"a clean-release leftover is not an orphan")
	require.Contains(t, stderr, "kept   released lock session-reap-released-loop.lock")
	require.Contains(t, stderr, "(dry-run; 2 lock(s) would be probed)")

	afterDryRun, err := os.Stat(crashPath)
	require.NoError(t, err)
	require.True(t, crashBefore.ModTime().Equal(afterDryRun.ModTime()), "dry-run must leave lock mtime unchanged")
	require.Equal(t, beforePID, session.ReadLockPID(crashPath), "dry-run must leave lock PID unchanged")
	heldAfterDryRun, err := os.Stat(heldPath)
	require.NoError(t, err)
	require.True(t, heldBefore.ModTime().Equal(heldAfterDryRun.ModTime()), "dry-run must not probe a live held lock")

	require.NoError(t, sessionsReapCmd.Flags().Set("dry-run", "false"))
	stderr = captureStderr(t, func() { require.NoError(t, sessionsReapCmd.RunE(sessionsReapCmd, nil)) })
	require.Contains(t, stderr, "reclaimed 1 lock(s)")
	require.FileExists(t, releasedLockPath, "the loop's lock file stays in place")
	after, err := os.Stat(releasedLockPath)
	require.NoError(t, err)
	require.True(t, before.ModTime().Equal(after.ModTime()), "not probed: the probe freshens the mtime")
	_, err = os.Stat(crashPath)
	require.True(t, os.IsNotExist(err), "the crash-orphan is reclaimed as before")
}
