package sesslive

// R8A-2 / R8C-2 (cmd part): `sessions cancel --all` used to flag every
// top-level session, idle web ones included, and the flag then aborted their
// next turn after one step. --all now flags only sessions with live work.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/PHPCraftdream/rush/internal/session"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func newLiveTestEnv(t *testing.T) (*app.App, string) {
	t.Helper()
	tmp := cmdtest.IsolateConfigEnvForTests(t)
	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(orig) })

	dataDir := filepath.Join(tmp, "sessions-live-data")
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	ensureSessionsLiveRootFlags(cmd, dataDir)
	a, err := cmdkit.SetupApp(cmd)
	require.NoError(t, err)
	require.Equal(t, dataDir, a.Config().Options.DataDirectory)
	t.Cleanup(func() {
		builtDataDir := a.Config().Options.DataDirectory
		a.Shutdown()
		_ = db.Release(builtDataDir)
	})
	return a, dataDir
}

func ensureSessionsLiveRootFlags(cmd *cobra.Command, dataDir string) {
	if cmd.Flags().Lookup("debug") == nil {
		cmd.Flags().Bool("debug", false, "")
	}
	if cmd.Flags().Lookup("data-dir") == nil {
		cmd.Flags().String("data-dir", "", "")
	}
	_ = cmd.Flags().Set("data-dir", dataDir)
}

func runCancel(t *testing.T, dataDir string, all bool, args ...string) (stderr string, err error) {
	t.Helper()
	cmd := NewSessionsCancelCmd()
	ensureSessionsLiveRootFlags(cmd, dataDir)
	if f := cmd.Flags().Lookup("cwd"); f == nil {
		cmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, cmd.Flags().Set("cwd", ""))
	require.NoError(t, cmd.Flags().Set("all", boolFlag(all)))
	cmd.SetContext(context.Background())
	stderr = cmdtest.CaptureStderr(t, func() { err = cmd.RunE(cmd, args) })
	return stderr, err
}

func claimLiveOwnJob(t *testing.T, a *app.App, owner, toolCallID string) {
	t.Helper()
	host, batches := cmdtest.BatchSeedHost(t, a)
	_, err := batches.Reserve(context.Background(), session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "claim-own-job-runner",
		ToolCallID: toolCallID, InputHash: "hash-" + toolCallID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "pending", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true},
			State: "pending", ControlState: "none",
		}}}},
	})
	require.NoError(t, err)
}

// Revert-check: flagging every listed session again (dropping the live-work
// filter) fails the idle rows; skipping the lock / job / delegation / driver
// signal fails that row.
func TestSessionsCancelAll_FlagsOnlySessionsWithLiveWork(t *testing.T) {
	a, dataDir := newLiveTestEnv(t)
	ctx := context.Background()
	mk := func(id string) string {
		sess, err := a.Sessions.CreateWithID(ctx, id, id)
		require.NoError(t, err)
		return sess.ID
	}
	idle, released := mk("cancel-idle"), mk("cancel-released")
	driven, owner, parent, locked := mk("cancel-driven"), mk("cancel-owner"), mk("cancel-parent"), mk("cancel-locked")
	cmdtest.AgeLock(t, cmdtest.ReleasedLock(t, dataDir, released), 2*time.Minute)
	child, err := a.Sessions.CreateTaskSession(ctx, "cancel-child", parent, "sub-agent")
	require.NoError(t, err)
	cmdtest.WriteLockFileAt(t, dataDir, locked, os.Getpid())

	driverHost := sessionhosts.NewHostStore(a.DB(), dataDir, os.Getpid(), "cancel-driver-fixture")
	driverHostID, err := driverHost.EnsureHost(ctx)
	require.NoError(t, err)
	require.NoError(t, driverHost.ClaimSessionDriver(ctx, driven))
	// A12 pattern: pin the host lock and keep the driver marker. The pinned
	// lock file stays open for the process lifetime; release it at test end
	// so the data dir can be removed on Windows.
	driverHost.CloseKeepLock()
	t.Cleanup(func() { sessionhosts.ReleaseRetainedHostLocksForTest(driverHostID) })
	claimLiveOwnJob(t, a, owner, "bash-1")
	cmdtest.ClaimDelegation(t, a, parent, "delegate-1", child.ID)
	a.Shutdown()

	stderr, err := runCancel(t, dataDir, true)
	require.NoError(t, err)
	require.Contains(t, stderr, "cancellation requested for 4 session(s)")
	require.Contains(t, stderr, "skipped 2 session(s) with no live work")

	conn, err := cmdtest.DBConnectForTest(t, dataDir)
	require.NoError(t, err)
	flagged := func(id string) bool {
		var v int
		require.NoError(t, conn.QueryRowContext(ctx, `SELECT cancel_requested FROM sessions WHERE id = ?`, id).Scan(&v))
		return v != 0
	}
	for _, id := range []string{driven, owner, parent, locked} {
		require.True(t, flagged(id), "%s has live work: flagged", id)
	}
	for _, id := range []string{idle, released} {
		require.False(t, flagged(id), "%s is idle: not flagged", id)
	}

	// A single id keeps flagging: the flag is a one-shot request the operator
	// asked for by name.
	stderr, err = runCancel(t, dataDir, false, idle)
	require.NoError(t, err)
	require.Contains(t, stderr, "cancellation requested for session "+idle)
	require.True(t, flagged(idle))
}

// boolFlag renders a bool for Flags().Set, the shape the deleted
// sessions_livework_test.go used to provide.
func boolFlag(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
