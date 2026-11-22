package sessclean

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	"github.com/PHPCraftdream/rush/internal/session"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// isolatedGcBatchOwnerEnv mirrors the former root sessions-gc fixture using
// an isolated config and a fresh command, so this package never shares Cobra
// flag state with the command's package-level instance.
func isolatedGcBatchOwnerEnv(t *testing.T) (a *app.App, shutdownSeed func(), cmd *cobra.Command) {
	t.Helper()
	tmp := cmdtest.IsolateConfigEnvForTests(t)
	dataDir := filepath.Join(tmp, "gc-jobs-data")

	ctx, cancel := context.WithCancel(context.Background())
	cmd = NewSessionsGcCmd(cmdkit.SetupApp)
	ensureRootFlagStandIns(cmd, dataDir)
	cmd.SetContext(ctx)
	for name, value := range map[string]string{
		"dry-run": "false", "older-than": "7d", "jobs-older-than": "",
		"max-sessions": "0", "json": "false",
	} {
		require.NoError(t, cmd.Flags().Set(name, value))
	}

	built, err := cmdkit.SetupApp(cmd)
	require.NoError(t, err)
	var once sync.Once
	shutdownSeed = func() {
		once.Do(func() {
			builtDataDir := built.Config().Options.DataDirectory
			built.Shutdown()
			_ = db.Release(builtDataDir)
		})
	}
	t.Cleanup(func() {
		shutdownSeed()
		cancel()
		waitForSQLiteHandleRelease(t, dataDir)
	})
	return built, shutdownSeed, cmd
}

// seedGcFinishedBatchOwner creates an old API service session with a real
// batch root and two task rows, all projected to a finished state.
func seedGcFinishedBatchOwner(t *testing.T, a *app.App) string {
	t.Helper()
	ctx := context.Background()
	const owner = "gc-batch-finished-owner"
	sess, err := a.Sessions.CreateWithID(ctx, owner, "gc batch fixture")
	require.NoError(t, err)
	_, err = a.DB().ExecContext(ctx,
		`UPDATE sessions SET created_at = ? WHERE id = ?`,
		time.Now().Add(-8*24*time.Hour).Unix(), sess.ID)
	require.NoError(t, err)
	_, err = a.DB().ExecContext(ctx,
		`UPDATE sessions SET origin = ? WHERE id = ?`, string(message.OriginAPIService), sess.ID)
	require.NoError(t, err)

	hostStore := sessionhosts.NewHostStore(a.DB(), a.Config().Options.DataDirectory, os.Getpid(), "gc-batch-owner-fixture")
	t.Cleanup(func() { _ = hostStore.Close(context.Background()) })
	host, err := hostStore.EnsureHost(ctx)
	require.NoError(t, err)
	_, err = session.NewBatchStore(a.DB()).Reserve(ctx, session.BatchReservation{
		Owner: sess.ID, Host: host, RunnerID: "batch-read-runner",
		ToolCallID: "batch-read-call", InputHash: "batch-read-hash", Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "pending", ControlState: "none",
		}, Children: []session.BatchReservationNode{
			{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "test", Valid: true}, State: "pending", ControlState: "none"}},
			{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "test", Valid: true}, State: "pending", ControlState: "none"}},
		}},
	})
	require.NoError(t, err)
	_, err = a.DB().ExecContext(ctx,
		`UPDATE batch_nodes SET state='completed', outcome_state='completed', outcome_code='ok' WHERE owner_session_id=?`, sess.ID)
	require.NoError(t, err)
	return sess.ID
}

func runGcBatchOwnerCommand(t *testing.T, cmd *cobra.Command) (string, string) {
	t.Helper()
	for name, value := range map[string]string{
		"dry-run": "false", "older-than": "7d", "jobs-older-than": "",
		"max-sessions": "0", "json": "false",
	} {
		require.NoError(t, cmd.Flags().Set(name, value))
	}
	var runErr error
	stdout, stderr := cmdtest.CaptureStdoutAndStderr(t, func() {
		runErr = cmd.RunE(cmd, nil)
	})
	require.NoError(t, runErr)
	return stdout, stderr
}
