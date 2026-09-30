package cmd

// Real-DB coverage for `sessions show`'s async job counts line (doc sec.5
// step 7): a session with a running and a terminal job shows both counts;
// a session with no jobs prints nothing extra.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestSessionsShowCmdRun_AsyncJobCounts(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)
	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })
	dataDir := filepath.Join(tmp, "show-jobs-data")

	ensureRootFlagStandIns(sessionsShowCmd, dataDir)
	sessionsShowCmd.SetContext(context.Background())
	require.NoError(t, sessionsShowCmd.Flags().Set("json", "false"))
	require.NoError(t, sessionsShowCmd.Flags().Set("with-messages", "false"))

	seed, err := setupApp(sessionsShowCmd)
	require.NoError(t, err)
	require.Equal(t, dataDir, seed.Config().Options.DataDirectory)
	t.Cleanup(func() { _ = db.Release(dataDir) })

	ctx := context.Background()
	sess, err := seed.Sessions.CreateWithID(ctx, "show-jobs-sess", "session with jobs")
	require.NoError(t, err)

	store := seed.AsyncJobStore()
	require.NotNil(t, store)
	_, err = store.Claim(ctx, session.ClaimParams{Owner: sess.ID, ToolCallID: "running-1", Kind: session.JobKindCommand, Input: "sleep 5"})
	require.NoError(t, err)
	_, err = store.Claim(ctx, session.ClaimParams{Owner: sess.ID, ToolCallID: "done-1", Kind: session.JobKindCommand, Input: "echo hi"})
	require.NoError(t, err)
	_, err = store.Transition(ctx, session.TransitionParams{Owner: sess.ID, ToolCallID: "done-1", State: "completed", NoticeKind: "completed", Wake: true, Delivery: "done"})
	require.NoError(t, err)

	// Detach + keep the seed store's own host lock alive across Shutdown, so
	// the running row still reads as alive once sessionsShowCmd.RunE builds
	// its own fresh App on the same data dir (mirrors sessions_jobs_test.go).
	seed.SetAsyncJobStoreForTest(nil)
	defer func() { _ = store.Close(context.Background()) }()
	seed.Shutdown()

	stdout := captureStdout(t, func() {
		require.NoError(t, sessionsShowCmd.RunE(sessionsShowCmd, []string{sess.ID}))
	})
	t.Logf("sessions show:\n%s", stdout)

	require.Contains(t, stdout, "Async jobs:   2 total, 1 running")
}

func TestSessionsShowCmdRun_NoAsyncJobsPrintsNothingExtra(t *testing.T) {
	tmp := isolateConfigEnvForTests(t)
	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })
	dataDir := filepath.Join(tmp, "show-nojobs-data")

	ensureRootFlagStandIns(sessionsShowCmd, dataDir)
	sessionsShowCmd.SetContext(context.Background())
	require.NoError(t, sessionsShowCmd.Flags().Set("json", "false"))
	require.NoError(t, sessionsShowCmd.Flags().Set("with-messages", "false"))

	seed, err := setupApp(sessionsShowCmd)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })

	ctx := context.Background()
	sess, err := seed.Sessions.CreateWithID(ctx, "show-nojobs-sess", "no jobs here")
	require.NoError(t, err)
	seed.Shutdown()

	stdout := captureStdout(t, func() {
		require.NoError(t, sessionsShowCmd.RunE(sessionsShowCmd, []string{sess.ID}))
	})
	require.NotContains(t, stdout, "Async jobs:")
}
