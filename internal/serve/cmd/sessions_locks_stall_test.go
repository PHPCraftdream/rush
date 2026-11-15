package cmd

// Revert-check documentation, one behavior per test:
//   - TestLocksStall_MarkerInPulseColumn: a lock whose heartbeat mtime is
//     past the stall threshold (10min) with a recorded holder PID must show
//     "STALLED" in its PULSE column, not a plain "offline"/"alive" label.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLocksStall_MarkerInPulseColumn(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	configuredDataDir := filepath.Join(tmp, "elsewhere-data")
	ensureRootFlagStandIns(sessionsLocksCmd, configuredDataDir)
	if f := sessionsLocksCmd.Flags().Lookup("cwd"); f == nil {
		sessionsLocksCmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, sessionsLocksCmd.Flags().Set("cwd", ""))
	require.NoError(t, sessionsLocksCmd.Flags().Set("json", "false"))
	require.NoError(t, sessionsLocksCmd.Flags().Set("stale-only", "false"))
	require.NoError(t, sessionsLocksCmd.Flags().Set("prune", "false"))
	sessionsLocksCmd.SetContext(context.Background())

	const sessionID = "stalled-locks-id"
	lockDir := filepath.Join(configuredDataDir, "locks")
	require.NoError(t, os.MkdirAll(lockDir, 0o755))
	lockPath := filepath.Join(lockDir, "session-"+sessionID+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644))
	old := time.Now().Add(-11 * time.Minute)
	require.NoError(t, os.Chtimes(lockPath, old, old))

	stdout := captureStdout(t, func() {
		runErr := sessionsLocksCmd.RunE(sessionsLocksCmd, nil)
		require.NoError(t, runErr)
	})

	require.Contains(t, stdout, "STALLED")
	require.NotContains(t, stdout, "(no locks)")
}
