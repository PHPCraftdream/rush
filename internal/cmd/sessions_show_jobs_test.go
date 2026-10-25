package cmd

// Real-DB coverage for `sessions show`'s batch work counts line: a session
// with a running and a terminal task shows both counts (under the
// batch_tasks_total/running JSON fields); a session with no
// batch work prints nothing extra.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestSessionsShowCmdRun_BatchTaskCounts(t *testing.T) {
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

	// The "one running, one terminal" shape: a live batch root with two task
	// children, one of them completed (raw UPDATE on rows Reserve created --
	// the established pattern).
	f := seedBatchReadRoot(t, seed, sess.ID)
	_, err = seed.DB().ExecContext(ctx, `UPDATE batch_nodes SET state = 'completed' WHERE node_id = ?`, f.nodeA)
	require.NoError(t, err)
	// The show command counts Running from Tasks.Active
	// (starting/running/cancelling), so nodeB must be RUNNING, not just
	// pending, for "2 total, 1 running".
	_, err = seed.DB().ExecContext(ctx, `UPDATE batch_nodes SET state = 'running' WHERE node_id = ?`, f.nodeB)
	require.NoError(t, err)

	// Keep the seed host's lock alive across Shutdown, so the running task
	// still reads as alive once sessionsShowCmd.RunE builds its own fresh
	// App on the same data dir (mirrors sessions_jobs_test.go).
	seed.Shutdown()
	// The fixture's HostStore stays open until cleanup: its held OS lock
	// keeps the host alive for the command run.

	stdout := captureStdout(t, func() {
		require.NoError(t, sessionsShowCmd.RunE(sessionsShowCmd, []string{sess.ID}))
	})
	t.Logf("sessions show:\n%s", stdout)

	require.Contains(t, stdout, "Batch tasks:  2 total, 1 running")
}

func TestSessionsShowCmdRun_NoBatchTasksPrintsNothingExtra(t *testing.T) {
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
	require.NotContains(t, stdout, "Batch tasks:")
}
