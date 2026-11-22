package sesslocks

// Test fixtures byte-copied from package cmd's root test files (rule 18):
// the moved locks tests need them, but sesslocks cannot import package cmd
// (import cycle: cmd imports sesslocks). Bodies are identical to their root
// originals except where noted.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// isolateConfigEnvForTests — byte-copy of sessions_reset_test.go's helper.
func isolateConfigEnvForTests(t *testing.T) (tmp string) {
	t.Helper()
	config.ResetProviderCacheForTests()

	tmp = t.TempDir()

	dataDir := filepath.Join(tmp, "global-data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)

	configDir := filepath.Join(tmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	rushlog.Setup("", false)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	return tmp
}

// ensureRootFlagStandIns — byte-copy of models_use_test.go's helper.
func ensureRootFlagStandIns(cmd *cobra.Command, dataDir string) {
	if f := cmd.Flags().Lookup("debug"); f == nil {
		cmd.Flags().Bool("debug", false, "")
	}
	if f := cmd.Flags().Lookup("data-dir"); f == nil {
		cmd.Flags().String("data-dir", "", "")
	}
	_ = cmd.Flags().Set("data-dir", dataDir)
}

// ageLock — byte-copy of sessions_activity_cmd_test.go's helper.
func ageLock(t *testing.T, path string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	require.NoError(t, os.Chtimes(path, old, old))
}

// waitOrDump — byte-copy of testwait_test.go's helper.
func waitOrDump(t *testing.T, ch <-chan struct{}, d time.Duration, what string) error {
	t.Helper()
	select {
	case <-ch:
		return nil
	case <-time.After(d):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Logf("goroutine dump after %s waiting for %s:\n%s", d, what, buf[:n])
		return fmt.Errorf("timed out after %s waiting for %s", d, what)
	}
}

// stopPurgeCmdContext — byte-copy of sessions_stop_purge_test.go's helper.
func stopPurgeCmdContext(t *testing.T, cmd *cobra.Command) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd.SetContext(ctx)
}

// fixtureSetupApp mirrors cmd_bridge.go's setupApp for fixture use: installs
// the config seams via cmdkit and opens the app on a plain carrier command
// (sesslocks cannot reference package cmd's command tree or bridge).
func fixtureSetupApp(cmd *cobra.Command) (*app.App, error) {
	cmdkit.UseCommandLoadOptions(func(c *cobra.Command) []config.LoadOption {
		return []config.LoadOption{config.WithCLIModels(cliprovider.AvailableCLIModels)}
	})
	return cmdkit.SetupApp(cmd)
}

// stopPurgeEnv — adapted from sessions_stop_purge_test.go: same isolation and
// seeding contract, but the app is opened on a local carrier command instead
// of package cmd's sessionsCancelCmd.
func stopPurgeEnv(t *testing.T) (a *app.App, dataDir string) {
	t.Helper()
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	dataDir = filepath.Join(tmp, "stop-purge-data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	cmd := &cobra.Command{}
	ensureRootFlagStandIns(cmd, dataDir)
	stopPurgeCmdContext(t, cmd)

	built, err := fixtureSetupApp(cmd)
	require.NoError(t, err)
	require.Equal(t, dataDir, built.Config().Options.DataDirectory,
		"test setup assumption: the command must resolve the configured data dir")

	t.Cleanup(func() {
		builtDataDir := built.Config().Options.DataDirectory
		built.Shutdown()
		_ = os.Chdir(orig)
		_ = db.Release(builtDataDir)
	})

	return built, dataDir
}

// seedQueuedWorkForStop — byte-copy of sessions_stop_purge_test.go's helper.
func seedQueuedWorkForStop(t *testing.T, a *app.App, sessionID string) (mergeMsgID, interruptMsgID string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, a.Sessions.EnqueueRunQueueEntry(ctx, "rq-x", sessionID, []byte(`{}`)))
	mergeMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "merge me"}},
	})
	require.NoError(t, err)
	interruptMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "interrupt me"}},
	})
	require.NoError(t, err)
	require.NoError(t, a.Sessions.CreatePendingInject(ctx, coresession.PendingInject{SessionID: sessionID, MessageID: mergeMsg.ID}))
	require.NoError(t, a.Sessions.CreatePendingInject(ctx, coresession.PendingInject{SessionID: sessionID, MessageID: interruptMsg.ID, Interrupt: true}))
	require.NoError(t, a.Sessions.WriteToOrphanOutbox(ctx, "ob-x", sessionID, []byte(`{}`)))
	return mergeMsg.ID, interruptMsg.ID
}

// writeLockFileAt — byte-copy of sessions_why_descendant_test.go's helper
// (SanitiseSessionIDForFilename is the exported sesslocks symbol).
func writeLockFileAt(t *testing.T, dataDir, sessionID string, pid int) string {
	t.Helper()
	locksDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))
	lockPath := filepath.Join(locksDir, "session-"+SanitiseSessionIDForFilename(sessionID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(strconv.Itoa(pid)+"\n"), 0o644))
	return lockPath
}

// backDateLock — byte-copy of sessions_why_descendant_test.go's helper.
func backDateLock(t *testing.T, path string) {
	t.Helper()
	stale := time.Now().Add(-(sessionlock.LockStaleDuration + 5*time.Second))
	require.NoError(t, os.Chtimes(path, stale, stale))
}

// releasedLock — byte-copy of sessions_activity_cmd_test.go's helper.
func releasedLock(t *testing.T, dataDir, sessionID string) string {
	t.Helper()
	path := writeLockFileAt(t, dataDir, sessionID, 0)
	require.NoError(t, os.WriteFile(path, nil, 0o644))
	backDateLock(t, path)
	return path
}

// acquireSessionLockForReset — byte-copy shim for the root package's
// unexported reset-path wrapper (moved here rule-18 with
// sessions_kill_sweep_unix_test.go, which calls it directly).
func acquireSessionLockForReset(dataDir, sessionID string, pid int, wait time.Duration) (*sessionlock.SessionLock, killResult, error) {
	return AcquireSessionLockForReset(dataDir, sessionID, pid, wait)
}

// sanitiseSessionIDForFilename — byte-copy shim of the root package's
// one-liner (it calls the exported sesslocks symbol here).
func sanitiseSessionIDForFilename(id string) string { return SanitiseSessionIDForFilename(id) }

// purgeQueuedWorkAfterStopForTest is the root purgeQueuedWorkAfterStop
// (sessions_stop_purge.go) as the kill command's purge provider in tests.
func purgeQueuedWorkAfterStopForTest(cmd *cobra.Command, sessionID string) {
	a, err := cmdkit.SetupAppLite(cmd)
	if err != nil {
		return
	}
	defer a.Shutdown()
	_, _ = a.Sessions.PurgeQueuedWorkForSession(cmd.Context(), sessionID)
}
