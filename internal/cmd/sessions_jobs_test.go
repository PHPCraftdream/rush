package cmd

// Real-DB, real-lock-file coverage for `rush sessions jobs`: table and
// --json output over live batch roots. Mirrors the
// isolatedListEnvWithConfiguredDataDir pattern (sessions_list_test.go): a
// SEED App reserves batch roots, then sessionsJobsCmd.RunE builds its OWN
// fresh App on the same data dir to read them back, exactly like a genuinely
// separate process would.

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// isolatedJobsEnv mirrors isolatedListEnvWithConfiguredDataDir but wires up
// sessionsJobsCmd instead.
func isolatedJobsEnv(t *testing.T) (a *app.App, dataDir string) {
	t.Helper()
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	dataDir = filepath.Join(tmp, "jobs-data")

	ctx, cancel := context.WithCancel(context.Background())
	ensureRootFlagStandIns(sessionsJobsCmd, dataDir)
	if f := sessionsJobsCmd.Flags().Lookup("json"); f == nil {
		sessionsJobsCmd.Flags().Bool("json", false, "")
	}
	require.NoError(t, sessionsJobsCmd.Flags().Set("json", "false"))
	sessionsJobsCmd.SetContext(ctx)

	built, err := setupApp(sessionsJobsCmd)
	require.NoError(t, err)
	require.Equal(t, dataDir, built.Config().Options.DataDirectory)

	t.Cleanup(func() {
		builtDataDir := built.Config().Options.DataDirectory
		built.Shutdown()
		_ = os.Chdir(orig)
		cancel()
		_ = db.Release(builtDataDir)
		waitForSQLiteHandleRelease(t, builtDataDir)
		waitForSQLiteHandleRelease(t, workDir)
	})

	return built, dataDir
}

func runJobsCmd(t *testing.T, args ...string) string {
	t.Helper()
	return captureStdout(t, func() {
		runErr := sessionsJobsCmd.RunE(sessionsJobsCmd, args)
		require.NoError(t, runErr)
	})
}

// TestSessionsJobsCmdRun_TableAndForeignLiveHost covers: a table listing a
// running batch root on a foreign live host, with no host-pid kill hint on
// the batch path.
func TestSessionsJobsCmdRun_TableAndForeignLiveHost(t *testing.T) {
	a, dataDir := isolatedJobsEnv(t)
	ctx := context.Background()

	sess, err := a.Sessions.CreateWithID(ctx, "jobs-table-sess", "session with jobs")
	require.NoError(t, err)

	foreignID := "foreign-live-host"
	foreignLock, err := filelock.TryAcquireFileLock(session.HostLockPath(dataDir, foreignID))
	require.NoError(t, err)
	t.Cleanup(func() { _ = foreignLock.Release() })
	q := db.New(a.DB())
	_, err = q.RegisterAsyncHostRow(ctx, db.RegisterAsyncHostRowParams{ID: foreignID, Pid: 4242, Label: "other-rush", StartedAt: 1700000000})
	require.NoError(t, err)
	reserved, err := session.NewBatchStore(a.DB()).Reserve(ctx, session.BatchReservation{
		Owner: sess.ID, Host: foreignID, RunnerID: "foreign-batch-runner",
		ToolCallID: "foreign-batch-call", InputHash: "foreign-batch-hash", Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none"}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, reserved.Snapshot.Root.RootID)
	a.Shutdown()
	stdout := runJobsCmd(t, sess.ID)
	lines := strings.Split(stdout, "\n")
	var foreignLine string
	for _, line := range lines {
		if strings.Contains(line, "foreign-batch-call") {
			foreignLine = line
		}
	}
	require.NotEmpty(t, foreignLine)
	require.NotContains(t, strings.Fields(foreignLine), "4242")
	require.Contains(t, stdout, "TOOL_CALL_ID")
	require.Contains(t, stdout, "foreign-batch-call")
	require.Contains(t, foreignLine, "running")
	require.Contains(t, foreignLine, "true")
	require.NotContains(t, stdout, "taskkill")
	require.NotContains(t, stdout, "kill -INT")
}

func TestSessionsJobsCmdRun_JSON(t *testing.T) {
	a, _ := isolatedJobsEnv(t)
	ctx := context.Background()

	sess, err := a.Sessions.CreateWithID(ctx, "jobs-json-sess", "session with jobs")
	require.NoError(t, err)

	f := seedBatchReadRoot(t, a, sess.ID)
	require.NoError(t, sessionsJobsCmd.Flags().Set("json", "true"))
	t.Cleanup(func() { _ = sessionsJobsCmd.Flags().Set("json", "false") })
	// The fixture's HostStore stays open until cleanup: its held OS lock
	// keeps the host alive for the command run.
	a.Shutdown()
	stdout := runJobsCmd(t, sess.ID)
	var item jobsJSONItem
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(stdout)), &item), stdout)
	require.Equal(t, f.rootID, item.BatchRootID)
	require.Equal(t, "batch-read-call", item.ToolCallID)
	require.Equal(t, "batch", item.Kind)
	require.Equal(t, "pending", item.State)
	require.True(t, item.BatchHeld)
	require.Equal(t, "held", item.Liveness)
	require.Empty(t, item.Hint)
	require.Zero(t, item.HostPID)
}

func TestSessionsJobsCmdRun_JSONDeadHostDeletesUnannouncedSubtree(t *testing.T) {
	a, dataDir := isolatedJobsEnv(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "jobs-json-dead-sess", "dead batch host")
	require.NoError(t, err)
	f := seedBatchReadRoot(t, a, sess.ID)
	var announced int
	require.NoError(t, a.DB().QueryRowContext(ctx, `SELECT announced FROM batch_nodes WHERE node_id = ?`, f.rootID).Scan(&announced))
	require.Zero(t, announced)
	var beforeCount int
	require.NoError(t, a.DB().QueryRowContext(ctx, `SELECT count(*) FROM batch_nodes WHERE root_id = ?`, f.rootID).Scan(&beforeCount))
	require.Equal(t, 3, beforeCount)
	// Release the fixture's host lock (A12: the closer is the only handle):
	// the probe must then read the host DEAD, like a crashed rush process.
	f.closeAsync()
	a.Shutdown()
	status, probeErr := session.ProbeHostShared(dataDir, f.host)
	require.Equal(t, session.HostStatusDead, status)
	require.NoError(t, probeErr)
	require.NoError(t, sessionsJobsCmd.Flags().Set("json", "true"))
	t.Cleanup(func() { _ = sessionsJobsCmd.Flags().Set("json", "false") })
	stdout := runJobsCmd(t, sess.ID)
	require.Empty(t, stdout)
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.ReleaseConn(conn)) }()
	var afterCount int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM batch_nodes WHERE root_id = ?`, f.rootID).Scan(&afterCount))
	require.Zero(t, afterCount)
}

// TestSessionsJobsCmdRun_NoJobs covers the empty case.
func TestSessionsJobsCmdRun_NoJobs(t *testing.T) {
	a, _ := isolatedJobsEnv(t)
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "jobs-empty-sess", "no jobs here")
	require.NoError(t, err)
	a.Shutdown()

	stdout := runJobsCmd(t, sess.ID)
	require.Contains(t, stdout, "no jobs")
}
