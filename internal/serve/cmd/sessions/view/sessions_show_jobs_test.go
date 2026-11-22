package sessview

// Real-DB coverage for `sessions show`'s batch work counts line: a session
// with a running and a terminal task shows both counts; a session with no
// batch work prints nothing extra.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/PHPCraftdream/rush/internal/session"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func ensureShowRootFlagStandIns(cmd *cobra.Command, dataDir string) {
	if cmd.Flags().Lookup("debug") == nil {
		cmd.Flags().Bool("debug", false, "")
	}
	if cmd.Flags().Lookup("data-dir") == nil {
		cmd.Flags().String("data-dir", "", "")
	}
	_ = cmd.Flags().Set("data-dir", dataDir)
}

func newShowCommand(t *testing.T, dataDir string) *cobra.Command {
	t.Helper()
	cmd := NewSessionsShowCmd(cmdkit.SetupApp)
	ensureShowRootFlagStandIns(cmd, dataDir)
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Flags().Set("json", "false"))
	require.NoError(t, cmd.Flags().Set("with-messages", "false"))
	return cmd
}

type showBatchFixture struct {
	rootID string
	nodeA  string
	nodeB  string
}

func seedShowBatchRoot(t *testing.T, a *app.App, owner string) showBatchFixture {
	t.Helper()
	ctx := context.Background()
	store := sessionhosts.NewHostStore(a.DB(), a.DataDirectory(), os.Getpid(), "show-batch-fixture")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	host, err := store.EnsureHost(ctx)
	require.NoError(t, err)
	reserved, err := session.NewBatchStore(a.DB()).Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "show-batch-runner",
		ToolCallID: "show-batch-call", InputHash: "show-batch-hash", Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "pending", ControlState: "none",
		}, Children: []session.BatchReservationNode{
			{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "test", Valid: true}, State: "pending", ControlState: "none"}},
			{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "test", Valid: true}, State: "pending", ControlState: "none"}},
		}},
	})
	require.NoError(t, err)
	var kids []string
	for _, node := range reserved.Snapshot.Nodes {
		if node.ParentID.Valid && node.ParentID.String == reserved.Snapshot.Root.RootID {
			kids = append(kids, node.NodeID)
		}
	}
	require.Len(t, kids, 2, "the fixture reserves exactly two task children")
	return showBatchFixture{rootID: reserved.Snapshot.Root.RootID, nodeA: kids[0], nodeB: kids[1]}
}

func TestSessionsShowCmdRun_BatchTaskCounts(t *testing.T) {
	tmp := cmdtest.IsolateConfigEnvForTests(t)
	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })
	dataDir := filepath.Join(tmp, "show-jobs-data")
	cmd := newShowCommand(t, dataDir)

	seed, err := cmdkit.SetupApp(cmd)
	require.NoError(t, err)
	require.Equal(t, dataDir, seed.Config().Options.DataDirectory)
	t.Cleanup(func() { _ = db.Release(dataDir) })

	ctx := context.Background()
	sess, err := seed.Sessions.CreateWithID(ctx, "show-jobs-sess", "session with jobs")
	require.NoError(t, err)

	fixture := seedShowBatchRoot(t, seed, sess.ID)
	_, err = seed.DB().ExecContext(ctx, `UPDATE batch_nodes SET state = 'completed' WHERE node_id = ?`, fixture.nodeA)
	require.NoError(t, err)
	// The show command counts Running from Tasks.Active (starting/running/cancelling).
	_, err = seed.DB().ExecContext(ctx, `UPDATE batch_nodes SET state = 'running' WHERE node_id = ?`, fixture.nodeB)
	require.NoError(t, err)

	// Keep the fixture host lock alive across Shutdown so the running task
	// remains live when the command opens its own App on the same data dir.
	seed.Shutdown()
	stdout := cmdtest.CaptureStdout(t, func() {
		require.NoError(t, cmd.RunE(cmd, []string{sess.ID}))
	})
	t.Logf("sessions show:\n%s", stdout)
	require.Contains(t, stdout, "Batch tasks:  2 total, 1 running")
}

func TestSessionsShowCmdRun_NoBatchTasksPrintsNothingExtra(t *testing.T) {
	tmp := cmdtest.IsolateConfigEnvForTests(t)
	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })
	dataDir := filepath.Join(tmp, "show-nojobs-data")
	cmd := newShowCommand(t, dataDir)

	seed, err := cmdkit.SetupApp(cmd)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })

	ctx := context.Background()
	sess, err := seed.Sessions.CreateWithID(ctx, "show-nojobs-sess", "no jobs here")
	require.NoError(t, err)
	seed.Shutdown()

	stdout := cmdtest.CaptureStdout(t, func() {
		require.NoError(t, cmd.RunE(cmd, []string{sess.ID}))
	})
	require.NotContains(t, stdout, "Batch tasks:")
}
