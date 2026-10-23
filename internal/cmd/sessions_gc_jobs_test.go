package cmd

// The seed environment for `sessions gc` command tests. The legacy
// async_jobs/session_notices retention fixtures were deleted with the old
// authority (obsolete contract: gc --jobs-older-than purged legacy
// async_jobs rows and reported an "async_jobs" JSON summary); the retained
// batch-side retention coverage lives in sessions_batch_act_test.go.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// isolatedGcEnv mirrors isolatedJobsEnv but wires up sessionsGcCmd. The seed
// app must be shut down (shutdownSeed) before sessionsGcCmdRun runs its own
// setupApp against the same data dir -- the process-wide MCP owner can only
// be held by one App at a time.
func isolatedGcEnv(t *testing.T) (a *app.App, shutdownSeed func()) {
	t.Helper()
	tmp := isolateConfigEnvForTests(t)
	dataDir := filepath.Join(tmp, "gc-jobs-data")

	ctx, cancel := context.WithCancel(context.Background())
	ensureRootFlagStandIns(sessionsGcCmd, dataDir)
	sessionsGcCmd.SetContext(ctx)
	require.NoError(t, sessionsGcCmd.Flags().Set("dry-run", "false"))
	require.NoError(t, sessionsGcCmd.Flags().Set("older-than", "7d"))
	require.NoError(t, sessionsGcCmd.Flags().Set("jobs-older-than", ""))
	require.NoError(t, sessionsGcCmd.Flags().Set("max-sessions", "0"))
	require.NoError(t, sessionsGcCmd.Flags().Set("json", "false"))

	built, err := setupApp(sessionsGcCmd)
	require.NoError(t, err)
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			builtDataDir := built.Config().Options.DataDirectory
			built.Shutdown()
			_ = db.Release(builtDataDir)
		})
	}
	t.Cleanup(func() {
		shutdown()
		cancel()
		waitForSQLiteHandleRelease(t, dataDir)
	})
	return built, shutdown
}
