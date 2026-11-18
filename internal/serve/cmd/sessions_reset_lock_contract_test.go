package cmd

// Lock-contract regression tests that drive the REAL root sessions reset
// command (sessionsResetCmd lives in root production sessions_reset.go);
// split out of the sesslocks-moved test files because sesslocks cannot
// import package cmd. Bodies are byte-identical to their originals.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/kit/sys/procinfo"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/stretchr/testify/require"
)

// TestSessionsReset_ForceLeavesLockFileInPlace enforces place 6: reset --force
// now acquires and HOLDS the OS lock across the DB reset, then releases — it
// deliberately does NOT remove the lock file (an empty lock file with no held
// OS lock is harmless; the next acquirer reopens and overwrites it). Old code
// unlinked the file, so this assertion fails on the old behavior.
func TestSessionsReset_ForceLeavesLockFileInPlace(t *testing.T) {
	a, cwd := isolatedResetEnv(t)

	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "reset-force-leaves-lock", "regression title")
	require.NoError(t, err)

	// Seed a bare lock file (stale PID, no live OS lock) exactly as
	// TestSessionsReset_ForceDoesNotKillStalePID does.
	lockDir := filepath.Join(cwd, ".rush", "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))

	require.True(t, procinfo.IsProcessAlive(os.Getpid()))
	// SessionsResetCmd.RunE creates its own full App. Release the seed App's
	// process-wide MCP owner before invoking the command.
	a.Shutdown()

	require.NoError(t, resetSessionCmdFlags().Flags().Set("force", "true"))
	stderr := cmdtest.CaptureStderr(t, func() {
		require.NoError(t, sessionsResetCmd.RunE(sessionsResetCmd, []string{sess.ID}))
	})
	t.Logf("reset --force stderr:\n%s", stderr)

	require.FileExists(t, lockPath,
		"reset --force must leave the lock file in place (held+released), not unlink it")
	require.True(t, procinfo.IsProcessAlive(os.Getpid()),
		"reset --force must never kill the calling test process via a stale lock-file PID")
}

// TestSessionsKillAndReset_RelativeDataDirResolveConsistently is the
// regression test for task #224 finding 1: sessionsKillCmdRun used to
// prefer the RAW --data-dir flag string over the config-resolved value
// (`dataDir := dataDirFlag; if dataDir == "" { ... }`). config.setDefaults
// resolves a relative data_directory against workingDir via
// filepathext.SmartJoin, and `sessions.go`'s reset --force block (via
// setupApp -> config.Init -> config.Load) always uses that fully-resolved
// value. So a RELATIVE --data-dir combined with a non-default --cwd used to
// make `sessions kill` resolve the lock path against the process's actual
// cwd (os.Getwd(), from before ResolveCwd's os.Chdir(--cwd) even if the
// working directory implied by --cwd never changed the shell's real cwd for
// unrelated reasons — the point is the two commands could disagree), while
// `sessions reset --force` resolved it against --cwd correctly — exactly
// the "wrong lock path, silent no-op" bug class task #219/6439f0f3 was
// supposed to have fixed, just for relative paths.
//
// This test drives both commands with the SAME relative --data-dir and the
// SAME non-default --cwd and asserts they resolve to the identical final
// absolute data directory, closing the divergence.
func TestSessionsKillAndReset_RelativeDataDirResolveConsistently(t *testing.T) {
	// Capture the real starting cwd BEFORE anything below calls
	// ResolveCwd(--cwd ...), which os.Chdir()s the whole process.
	orig, err := os.Getwd()
	require.NoError(t, err)

	tmp := isolateConfigEnvForTests(t)

	// Register the chdir-restore cleanup AFTER isolateConfigEnvForTests's
	// own t.TempDir() call above: t.Cleanup runs LIFO, so this must run
	// BEFORE tmp's TempDir cleanup tries to RemoveAll(tmp) — on Windows a
	// directory that is still the process's cwd cannot be deleted, and
	// projectDir (created under tmp below) is exactly what --cwd chdirs
	// into.
	t.Cleanup(func() { _ = os.Chdir(orig) })

	// The process's actual OS cwd is deliberately left as whatever the test
	// binary started in (or wherever a prior test in this file chdir'd to);
	// both commands are pointed at a project dir via --cwd instead, so a
	// resolution that (incorrectly) ignores --cwd and uses the process's
	// raw os.Getwd() would resolve to a visibly different, wrong path.
	projectDir := filepath.Join(tmp, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	const relativeDataDir = "relative-data-dir"
	wantDataDir := filepath.Clean(filepath.Join(projectDir, relativeDataDir))

	// --- sessions kill's resolution: exercised through the REAL RunE, not
	// a hand-rolled reimplementation of its resolution logic — otherwise
	// this test could pass even if sessionsKillCmdRun regressed, since it
	// wouldn't be calling the code under test at all. Mirrors
	// TestSessionsKillCmdRun_HonorsConfiguredDataDir's approach: seed a
	// real lock file at the path the FIX computes (wantDataDir), run the
	// real command, and assert it found (and removed) that lock rather
	// than reporting "no lock file at <wrong, cwd-based path>".
	const sessionID = "relative-datadir-consistency-id"
	lockDir := filepath.Join(wantDataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))

	ensureRootFlagStandIns(sessionsKillCmd, relativeDataDir)
	if f := sessionsKillCmd.Flags().Lookup("cwd"); f == nil {
		sessionsKillCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsKillCmd.Flags().Set("cwd", projectDir))
	// sessionsKillCmd is a package-level var shared across every test in
	// this file/binary. Reset --cwd back to "" once this test is done so a
	// later test never inherits a --cwd pointing at this test's (by then
	// deleted) projectDir — this bit a sibling package test
	// (TestSessionsReset_Force*) the first time this test was written,
	// since projectDir no longer exists once t.TempDir() cleans it up.
	t.Cleanup(func() { _ = sessionsKillCmd.Flags().Set("cwd", "") })
	require.NoError(t, sessionsKillCmd.Flags().Set("keep-lock", "false"))
	require.NoError(t, sessionsKillCmd.Flags().Set("wait", "2s"))
	sessionsKillCmd.SetContext(context.Background())

	killStderr := cmdtest.CaptureStderr(t, func() {
		runErr := sessionsKillCmd.RunE(sessionsKillCmd, []string{sessionID})
		require.NoError(t, runErr)
	})
	t.Logf("sessions kill stderr:\n%s", killStderr)

	require.NotContains(t, killStderr, "no lock file at",
		"sessions kill must resolve a relative --data-dir against --cwd, not the process's raw cwd")
	require.Contains(t, killStderr, wantDataDir,
		"sessions kill's report must reference the --cwd-resolved data dir, not a cwd-based guess")
	_, statErr := os.Stat(lockPath)
	require.True(t, os.IsNotExist(statErr), "lock file at the --cwd-resolved data dir must be removed")

	// --- sessions reset --force's resolution (via the real setupApp path) ---
	ensureRootFlagStandIns(sessionsResetCmd, relativeDataDir)
	if f := sessionsResetCmd.Flags().Lookup("cwd"); f == nil {
		sessionsResetCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsResetCmd.Flags().Set("cwd", projectDir))
	// Same package-level-shared-command hazard as sessionsKillCmd above:
	// reset --cwd back to "" so isolatedResetEnv/isolatedResetEnvWithConfiguredDataDir
	// (which leave --cwd unset, relying on ResolveCwd's os.Getwd() fallback)
	// never inherit this test's now-deleted projectDir.
	t.Cleanup(func() { _ = sessionsResetCmd.Flags().Set("cwd", "") })
	ctx, cancel := context.WithCancel(context.Background())
	sessionsResetCmd.SetContext(ctx)

	built, err := setupApp(sessionsResetCmd)
	require.NoError(t, err)
	t.Cleanup(func() {
		builtDataDir := built.Config().Options.DataDirectory
		built.Shutdown()
		cancel()
		_ = db.Release(builtDataDir)
		waitForSQLiteHandleRelease(t, builtDataDir)
	})

	// Both commands must resolve the SAME relative --data-dir + --cwd
	// combination to the identical absolute path (wantDataDir): sessions
	// kill's real RunE was already asserted above (via the lock file it
	// found and removed) to have operated against wantDataDir; this
	// asserts sessions reset --force's real resolution (setupApp's
	// config.Init) lands on the exact same value, closing the divergence.
	require.Equal(t, wantDataDir, built.Config().Options.DataDirectory,
		"sessions kill and sessions reset --force must resolve the same relative --data-dir to the same absolute path")
}

// TestReacquireAfterKill_WaitsOutLingeringOSLock: the OS may release a killed
// holder's lock after the PID already reads dead (LockFileEx under load). A
// second in-process handle reproduces "busy for a while, then free"; the
