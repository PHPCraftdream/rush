package sessions

// Real-DB, real-lock-file coverage for `rush sessions jobs`: table and
// --json output over live batch roots. The tests share the sessions package's
// command fixtures and run the jobs command from a fresh NewSessionsCmd tree.

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/kit/fs/filelock"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/PHPCraftdream/rush/internal/session"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/stretchr/testify/require"
)

// TestSessionsJobsCmdRun_TableAndForeignLiveHost covers: a table listing a
// running batch root on a foreign live host, with no host-pid kill hint on
// the batch path.
func TestSessionsJobsCmdRun_TableAndForeignLiveHost(t *testing.T) {
	jobsCmd, _, err := NewSessionsCmd().Find([]string{"jobs"})
	require.NoError(t, err)
	a, _, dataDir := isolatedCommandEnv(t, jobsCmd, "jobs-data")
	ctx := context.Background()

	sess, err := a.Sessions.CreateWithID(ctx, "jobs-table-sess", "session with jobs")
	require.NoError(t, err)

	foreignID := "foreign-live-host"
	foreignLock, err := filelock.TryAcquireFileLock(sessionhosts.HostLockPath(dataDir, foreignID))
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
	stdout := cmdtest.CaptureStdout(t, func() {
		require.NoError(t, jobsCmd.RunE(jobsCmd, []string{sess.ID}))
	})
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
	jobsCmd, _, err := NewSessionsCmd().Find([]string{"jobs"})
	require.NoError(t, err)
	a, _, _ := isolatedCommandEnv(t, jobsCmd, "jobs-data")
	ctx := context.Background()

	sess, err := a.Sessions.CreateWithID(ctx, "jobs-json-sess", "session with jobs")
	require.NoError(t, err)

	f := seedBatchReadRoot(t, a, sess.ID)
	require.NoError(t, jobsCmd.Flags().Set("json", "true"))
	t.Cleanup(func() { _ = jobsCmd.Flags().Set("json", "false") })
	// The fixture's HostStore stays open until cleanup: its held OS lock
	// keeps the host alive for the command run.
	a.Shutdown()
	stdout := cmdtest.CaptureStdout(t, func() {
		require.NoError(t, jobsCmd.RunE(jobsCmd, []string{sess.ID}))
	})
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
	jobsCmd, _, err := NewSessionsCmd().Find([]string{"jobs"})
	require.NoError(t, err)
	a, _, dataDir := isolatedCommandEnv(t, jobsCmd, "jobs-data")
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
	status, probeErr := sessionhosts.ProbeHostShared(dataDir, f.host)
	require.Equal(t, session.HostStatusDead, status)
	require.NoError(t, probeErr)
	require.NoError(t, jobsCmd.Flags().Set("json", "true"))
	t.Cleanup(func() { _ = jobsCmd.Flags().Set("json", "false") })
	stdout := cmdtest.CaptureStdout(t, func() {
		require.NoError(t, jobsCmd.RunE(jobsCmd, []string{sess.ID}))
	})
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
	jobsCmd, _, err := NewSessionsCmd().Find([]string{"jobs"})
	require.NoError(t, err)
	a, _, _ := isolatedCommandEnv(t, jobsCmd, "jobs-data")
	ctx := context.Background()
	sess, err := a.Sessions.CreateWithID(ctx, "jobs-empty-sess", "no jobs here")
	require.NoError(t, err)
	a.Shutdown()

	stdout := cmdtest.CaptureStdout(t, func() {
		require.NoError(t, jobsCmd.RunE(jobsCmd, []string{sess.ID}))
	})
	require.Contains(t, stdout, "no jobs")
}
