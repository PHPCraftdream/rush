package sesslive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/kit/sys/procinfo"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/stretchr/testify/require"
)

func TestSessionsReset_ForceLeavesLockFileInPlace(t *testing.T) {
	a, cmd, dataDir, shutdown := resetTestEnv(t, "", "")
	sess, err := a.Sessions.CreateWithID(context.Background(), "reset-force-leaves-lock", "regression title")
	require.NoError(t, err)
	lockPath := resetLockPath(dataDir, sess.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(lockPath), 0o755))
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))
	require.True(t, procinfo.IsProcessAlive(os.Getpid()))
	shutdown()
	stderr := runResetCommand(t, cmd, sess.ID, true)
	t.Logf("reset --force stderr:\n%s", stderr)
	require.FileExists(t, lockPath, "reset --force must leave the lock file in place (held+released), not unlink it")
	require.True(t, procinfo.IsProcessAlive(os.Getpid()), "reset --force must never kill the calling test process via a stale lock-file PID")
}

func TestSessionsKillAndReset_RelativeDataDirResolveConsistently(t *testing.T) {
	orig, err := os.Getwd()
	require.NoError(t, err)
	tmp := cmdtest.IsolateConfigEnvForTests(t)
	t.Cleanup(func() { _ = os.Chdir(orig) })
	projectDir := filepath.Join(tmp, "project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	const relativeDataDir = "relative-data-dir"
	wantDataDir := filepath.Clean(filepath.Join(projectDir, relativeDataDir))

	// The kill and reset commands are independently constructed production
	// commands with independent RunE resolution paths. Kill must find/remove
	// its lock through ResolveDataDirectory; reset must find/hold/release its
	// own lock through SetupApp's resolved config data directory.
	a, resetCmd, resolvedDir, shutdown := resetTestEnv(t, relativeDataDir, projectDir)
	require.Equal(t, wantDataDir, resolvedDir,
		"sessions kill and sessions reset --force must resolve the same relative --data-dir to the same absolute path")
	sess, err := a.Sessions.CreateWithID(context.Background(), "relative-datadir-consistency-id", "reset path")
	require.NoError(t, err)
	shutdown()
	lockPath := resetLockPath(wantDataDir, sess.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(lockPath), 0o755))
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))

	killCmd := newResetTestKillCmd(t, relativeDataDir, projectDir)
	killStderr := cmdtest.CaptureStderr(t, func() {
		require.NoError(t, killCmd.RunE(killCmd, []string{sess.ID}))
	})
	t.Logf("sessions kill stderr:\n%s", killStderr)
	require.NotContains(t, killStderr, "no lock file at", "sessions kill must resolve relative --data-dir against --cwd")
	require.Contains(t, killStderr, wantDataDir, "sessions kill must operate on its independently resolved configured path")
	_, statErr := os.Stat(lockPath)
	require.True(t, os.IsNotExist(statErr), "sessions kill must remove the lock at the --cwd-resolved data dir")

	// Re-seed a stale PID and exercise reset's real command path separately.
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))
	stderr := runResetCommand(t, resetCmd, sess.ID, true)
	t.Logf("sessions reset stderr:\n%s", stderr)
	require.Contains(t, stderr, wantDataDir, "sessions reset must use its setupApp-resolved data dir")
	require.FileExists(t, lockPath, "reset acquires and releases but does not unlink the lock file")
	require.True(t, procinfo.IsProcessAlive(os.Getpid()), "stale lock metadata must never kill the test process")

	// Keep this import exercised for the explicit pool release contract used
	// by cleanup on Windows after both command paths close their apps.
	_ = db.Release(resolvedDir)
	_ = cmdkit.SetupAppLite
}
