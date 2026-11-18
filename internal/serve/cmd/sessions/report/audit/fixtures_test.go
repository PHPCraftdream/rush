package sessaudit

// Test fixtures byte-copied from the root cmd package (rule 18): helpers the
// moved audit tests need on BOTH sides — the staying root tests keep their
// own untouched copies.

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	"github.com/PHPCraftdream/rush/internal/session"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// isolateConfigEnvForTests sets XDG_DATA_HOME/RUSH_GLOBAL_DATA,
// XDG_CONFIG_HOME/RUSH_GLOBAL_CONFIG, and RUSH_PROVIDER_CACHE_ONLY to
// throwaway directories under a fresh t.TempDir(), and points the default
// slog logger at io.Discard — the isolation this repo's CLAUDE.md documents
// as mandatory (not optional) for any test that reaches config.Load /
// config.Init / config.ResolveDataDirectory, since those read real config
// paths from the environment (GlobalConfig / GlobalConfigData) unless
// explicitly isolated. Without this, a test can silently read from or write
// to the operator's real global config/data — confirmed to have happened
// before.
//
// Returns the base tmp dir so callers can derive their own subdirectories
// (e.g. a configured --data-dir that lives outside it, or a workDir to
// os.Chdir into) without colliding with the env dirs this helper already
// created.
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

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// short mirrors the root cmd package's short (cmd_bridge.go): the moved
// audit tests reference it, and the audit report prints truncated session
// ids the same way.
func short(hash string) string { return cliout.Short(hash) }

// ---- byte-copied root fixtures needed by the moved batch-audit tests ----

// batchReadFixture carries the seeded root's identities for assertions.
// closeAsync closes the seed's host store (releasing its host lock).
type batchReadFixture struct {
	rootID, nodeA, nodeB, host, owner, toolCallID string
	store                                         *session.BatchStore
	closeAsync                                    func()
}

// seedBatchReadRoot reserves a real batch root (kind=batch with tool call and
// input hash, two task children) for owner on a host registered by this
// live test process (sessionhosts.HostStore.EnsureHost), so the live reader
// proves liveness via the own-host registry. The host store is closed at
// cleanup so the host lock cannot block later command runs.
func seedBatchReadRoot(t *testing.T, a *app.App, owner string) batchReadFixture {
	t.Helper()
	ctx := context.Background()
	store := sessionhosts.NewHostStore(a.DB(), a.DataDirectory(), os.Getpid(), "batch-read-fixture")
	closeAsync := func() { _ = store.Close(context.Background()) }
	t.Cleanup(closeAsync)
	host, err := store.EnsureHost(ctx)
	require.NoError(t, err)
	bs := session.NewBatchStore(a.DB())
	reserved, err := bs.Reserve(ctx, session.BatchReservation{
		Owner: owner, Host: host, RunnerID: "batch-read-runner",
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
	// Snapshot.Nodes includes the root and is ordered by node_id: derive the
	// task children by parent_id, never by index.
	rootID := reserved.Snapshot.Root.RootID
	var kids []string
	for _, n := range reserved.Snapshot.Nodes {
		if n.ParentID.Valid && n.ParentID.String == rootID {
			kids = append(kids, n.NodeID)
		}
	}
	require.Len(t, kids, 2, "the fixture reserves exactly two task children")
	return batchReadFixture{
		rootID: rootID,
		nodeA:  kids[0],
		nodeB:  kids[1],
		host:   host, owner: owner, toolCallID: "batch-read-call", store: bs,
		closeAsync: closeAsync,
	}
}

// auditJobsLoadOptionsOnce installs a nil-safe config option builder once:
// the root cmd package installs its own (CLI models + discovery scope);
// this package only ever opens plain data-dir-scoped apps in tests.
var auditJobsLoadOptionsOnce sync.Once

func auditJobsLoadOptionsOnceInstall() {
	auditJobsLoadOptionsOnce.Do(func() {
		cmdkit.UseCommandLoadOptions(func(*cobra.Command) []config.LoadOption { return nil })
	})
}

// isolatedJobsEnv byte-copied from the root cmd package (sessions_jobs_test.go,
// rule 18), pointed at a fresh throwaway command instead of the root-only
// sessionsJobsCmd: the moved batch-audit tests only need a live App on an
// isolated data dir to seed batch roots through.
func isolatedJobsEnv(t *testing.T) (a *app.App, dataDir string) {
	t.Helper()
	auditJobsLoadOptionsOnceInstall()
	tmp := isolateConfigEnvForTests(t)

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	dataDir = filepath.Join(tmp, "jobs-data")

	ctx, cancel := context.WithCancel(context.Background())
	auditJobsCmd := &cobra.Command{Use: "jobs"}
	ensureRootFlagStandIns(auditJobsCmd, dataDir)
	require.NoError(t, auditJobsCmd.Flags().Set("json", "false"))
	auditJobsCmd.SetContext(ctx)

	built, err := cmdkit.SetupApp(auditJobsCmd)
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

// ensureRootFlagStandIns registers debug/data-dir flags directly on cmd if
// not already present (idempotent across the package-level command vars
// reused by multiple tests), and points data-dir at tmp. cwd is intentionally
// left unset here — ResolveCwd falls back to os.Getwd(), which
// isolatedJobsEnv has already chdir'd into workDir, so omitting it is
// equivalent to passing it explicitly and one fewer flag to reset.
func ensureRootFlagStandIns(cmd *cobra.Command, dataDir string) {
	if f := cmd.Flags().Lookup("debug"); f == nil {
		cmd.Flags().Bool("debug", false, "")
	}
	if f := cmd.Flags().Lookup("data-dir"); f == nil {
		cmd.Flags().String("data-dir", "", "")
	}
	_ = cmd.Flags().Set("data-dir", dataDir)
}

// waitForSQLiteHandleRelease byte-copied from the root cmd package
// (models_use_test.go, rule 18): on Windows a still-open SQLite handle blocks
// t.TempDir() cleanup, so the real removal is retried for a budget.
func waitForSQLiteHandleRelease(t *testing.T, dataDir string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := os.RemoveAll(dataDir); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(25 * time.Millisecond)
	}
	if lastErr != nil {
		t.Logf("waitForSQLiteHandleRelease: %s still not removable after budget (%v); proceeding anyway", dataDir, lastErr)
	}
}

// TestMain isolates global state for this package's test binary (same as the other cmd packages).
func TestMain(m *testing.M) { cmdtest.Main(m) }

// The tests drive the package-private template command; its flags are
// registered by the factory (once), which only the cmd root called before the
// package move.
func init() { _ = NewSessionsAuditCmd() }
