package sesslive

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	locks "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/live/locks"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/stretchr/testify/require"
)

// TestIsSessionFinished_HonorsConfiguredDataDir is the regression test for
// task #233 finding 3: `sessions watch` used to look for the session lock at
// filepath.Join(cwd, ".rush", "locks"), ignoring --data-dir / a configured
// data_directory. The lock lookup now comes from the app's own configuration
// (App.SessionActivity), so the property is pinned where it lives: an app
// booted with a data directory outside the cwd must see a live lock seeded
// ONLY there, and must not report the session finished although its
// ended_reason is set.
//
// The earlier version of this test drove the whole command and cancelled it
// after 10 s; the watch start grace (watchStartGrace, 15 s) holds a
// finished-looking session open for just as long when the lock is missed, so
// that test also passed with the lock lookup pointed at the wrong directory.
//
// Revert-check: pointing session.InspectSessionLockFact in
// internal/app/app_session_activity.go at another directory makes lockAlive
// false and this test fail.
func TestIsSessionFinished_HonorsConfiguredDataDir(t *testing.T) {
	tmp := cmdtest.IsolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	// Deliberately outside workDir entirely, so filepath.Join(cwd, ".rush")
	// (the pre-fix hardcoded guess) can never accidentally coincide with it.
	configuredDataDir := filepath.Join(tmp, "elsewhere-data")
	cmd := NewSessionsWatchCmd()
	cmd.Flags().String("data-dir", "", "")
	cmd.Flags().Bool("debug", false, "")
	cmd.Flags().StringP("cwd", "c", "", "")
	require.NoError(t, cmd.Flags().Set("data-dir", configuredDataDir))
	require.NoError(t, cmd.Flags().Set("cwd", ""))

	ctx := context.Background()
	cmd.SetContext(ctx)

	built, err := cmdkit.SetupApp(cmd)
	require.NoError(t, err)
	require.Equal(t, configuredDataDir, built.Config().Options.DataDirectory,
		"test setup assumption: resolved DataDirectory must equal the --data-dir we configured")
	t.Cleanup(func() {
		dataDir := built.Config().Options.DataDirectory
		built.Shutdown()
		_ = db.Release(dataDir)
		waitResetSQLiteHandleRelease(t, dataDir)
	})

	sess, err := built.Sessions.CreateWithID(ctx, "watch-configured-datadir-id", "regression title")
	require.NoError(t, err)
	require.NoError(t, built.Sessions.SetEndedReason(ctx, sess.ID, "max_cost"))

	// Sanity: with no lock anywhere the ended_reason alone ends the watch.
	st, reason := isSessionFinished(ctx, built, sess.ID)
	require.False(t, st.lockAlive)
	require.True(t, st.done, "setup assumption: a set ended_reason without a live lock is finished")
	require.Equal(t, "max_cost", reason)

	// Live-looking lock ONLY at the configured (non-cwd) data dir.
	lockPath := cmdtest.WriteLockFileAt(t, configuredDataDir, sess.ID, os.Getpid())
	now := time.Now()
	require.NoError(t, os.Chtimes(lockPath, now, now))

	// The WRONG (pre-fix) path must not exist.
	wrongPath := filepath.Join(workDir, ".rush", "locks", "session-"+locks.SanitiseSessionIDForFilename(sess.ID)+".lock")
	_, wrongStatErr := os.Stat(wrongPath)
	require.True(t, os.IsNotExist(wrongStatErr))

	st, _ = isSessionFinished(ctx, built, sess.ID)
	require.True(t, st.lockAlive, "the lock at the configured data dir must be seen as alive")
	require.False(t, st.done, "an alive lock at the configured data dir must keep the watch running despite ended_reason")
}
