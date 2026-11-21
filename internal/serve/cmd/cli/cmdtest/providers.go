package cmdtest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

// RunProvidersCmdInIsolatedAppFull executes a real providers subcommand's
// RunE against an isolated config fixture, capturing real stdout. It stands
// up a full app via setupApp (the same path the CLI uses) in a temp data dir
// with network/provider-discovery disabled, so the output is produced by the
// real rendering code in providers.go — not a reimplementation.
//
// cmd is the real providersShowCmd/providersListCmd/providersSetCmd/
// providersAddCmd. providerJSON is the raw JSON for the "providers" object
// written into the isolated global rush.json before the command runs. args
// is the positional/flag payload parsed onto cmd (e.g. "with-peak" for show,
// "--json" for list, "with-peak --peak-hours 10:00-20:00" for a write
// command).
//
// dataDir selects the config layout the command runs under — pass "" or a
// relative suffix such as ".rush":
//
//   - "" keeps the historical behaviour: the carrier's --data-dir is the
//     isolated global data dir (tmp), so the workspace scope collapses onto
//     the SAME file as the global scope (both resolve to
//     <tmp>/rush.json — global is RUSH_GLOBAL_DATA, workspace is
//     <DataDirectory>/rush.json). workspacePath is returned as "" because
//     the two are indistinguishable; use this for single-scope tests.
//   - ".rush" (production layout <cwd>/.rush) points the carrier's
//     --data-dir at <workDir>/.rush, which makes the workspace scope a
//     DIFFERENT file from the global one. Use this when a test passes
//     --local and must read back / distinguish the workspace file from the
//     untouched global one.
//
// Returns the captured stdout, the global rush.json path
// (RUSH_GLOBAL_DATA/rush.json) and the workspace rush.json path — "" when
// dataDir is "", otherwise <dataDir>/rush.json, exactly what
// configPath(ScopeWorkspace) resolves to (workspace = <DataDirectory>/
// rush.json, appName "rush"). runErr is the raw RunE error, RETURNED rather
// than failed on so callers can assert on it.
func RunProvidersCmdInIsolatedAppFull(t *testing.T, cmd *cobra.Command, providerJSON, args, dataDir string) (stdout, globalPath, workspacePath string, runErr error) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	t.Setenv("RUSH_GLOBAL_DATA", tmp)
	// GlobalConfig() (RUSH_GLOBAL_CONFIG/XDG_CONFIG_HOME) is a SEPARATE
	// resolution path from GlobalConfigData() (RUSH_GLOBAL_DATA) above — see
	// CLAUDE.md's "two real config paths" caveat and the longer explanation
	// in isolatedModelsEnv (models_use_test.go). Without this, setupApp's
	// app.New() -> mcp.Initialize reads the real host
	// ~/.config/rush/rush.json and, if it configures MCP servers, tries to
	// open real network connections to them from inside the test — this is
	// the exact path that previously hung a stress run for 9+ minutes.
	//
	// Use a SEPARATE subdirectory from RUSH_GLOBAL_DATA (not the same tmp)
	// so lookupConfigs (internal/config/load.go), which loads and merges
	// both GlobalConfig() and GlobalConfigData(), doesn't load the same file
	// path twice under two different env vars — see the "Low, latent"
	// duplicate-merge caveat fixed in isolatedModelsEnv.
	configDir := filepath.Join(tmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	// Cache-only so provider discovery makes no network calls.
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	// Pre-initialise the once-only global logger so setupApp's log.Setup
	// call is a no-op and does not open a lumberjack handle inside the
	// temp dir (which would lock the file and break t.TempDir cleanup).
	rushlog.Setup("", false)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	globalDataPath := filepath.Join(tmp, "rush.json")
	require.NoError(t, os.WriteFile(globalDataPath, []byte(providerJSON), 0o644))

	// setupApp reads debug/data-dir/cwd off the command it receives. Those
	// are normally rootCmd persistent flags; build a carrier command that
	// carries them so we can invoke the real subcommand's RunE directly.
	ctx, cancel := context.WithCancel(context.Background())
	// dataDir == "" collapses the workspace scope onto the global file, so
	// the pooled SQLite connection lives directly under tmp. Otherwise it
	// lives under the workspace data dir (<workDir>/<dataDir>).
	dbDir := tmp
	if dataDir != "" {
		dbDir = filepath.Join(workDir, dataDir)
	}
	workspaceDataPath := filepath.Join(dbDir, "rush.json")
	t.Cleanup(func() {
		_ = os.Chdir(orig)
		// setupApp opens a pooled SQLite connection under the data dir;
		// cancel ctx and release THIS test's own connection so t.TempDir
		// cleanup doesn't hit a locked rush.db / rush.log on Windows.
		//
		// db.Release(dir), not db.ResetPool(): this file alone has ~19
		// t.Parallel() tests sharing this helper. ResetPool() used to nuke
		// the ENTIRE process-wide connection pool, including any other
		// still-running parallel test's live connection to a different
		// data dir — real cross-test interference, not just OS-level
		// handle-release lag, and a genuine contributor to this package's
		// Windows-only "process cannot access the file" flakiness.
		cancel()
		_ = db.Release(dbDir)
	})
	carrier := &cobra.Command{Use: "rush"}
	carrier.Flags().Bool("debug", false, "")
	carrier.Flags().String("data-dir", dbDir, "")
	carrier.Flags().String("cwd", workDir, "")
	carrier.SetContext(ctx)

	// Reset EVERY flag of the subcommand so state from a prior invocation in
	// the same process doesn't leak in — pflag's Changed otherwise survives
	// between tests, and since these command values are package-level
	// singletons (e.g. a prior test's --peak-hours) would corrupt a later
	// message-only test. DefValue is the registered default.
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	})
	cmd.SetArgs(nil)

	var runArgs []string
	if args != "" {
		runArgs = strings.Fields(args)
	}
	require.NoError(t, cmd.ParseFlags(runArgs))
	// cmd.RunE below is invoked as cmd.RunE(carrier, runArgs) — carrier,
	// not cmd, so setupAppLite(cmd) inside RunE (whose "cmd" parameter is
	// carrier at call time) can read the debug/data-dir/cwd flags only
	// carrier defines. But every subcommand-owned flag (peak-hours,
	// api-key, json, ...) was just parsed onto cmd's OWN FlagSet above —
	// without merging it in here, RunE's Flags().Changed/Get* calls would
	// silently see carrier's empty FlagSet instead. AddFlagSet shares the
	// underlying *pflag.Flag values (not copies), so carrier sees the
	// exact Value/Changed state cmd.ParseFlags just set.
	carrier.Flags().AddFlagSet(cmd.Flags())

	// Capture os.Stdout — providers list/show write there directly.
	var buf bytes.Buffer
	oldOut := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, r); close(done) }()

	// RunE receives the positional arguments only, as under cobra: flags
	// such as --all were already parsed onto cmd above.
	runErr = cmd.RunE(carrier, cmd.Flags().Args())

	_ = w.Close()
	os.Stdout = oldOut
	<-done

	if dataDir == "" {
		// The workspace scope collapses onto the global file — both resolve
		// to tmp/rush.json — so there is no distinct path to return.
		return buf.String(), globalDataPath, "", runErr
	}
	return buf.String(), globalDataPath, workspaceDataPath, runErr
}

// RunProvidersCmdInIsolatedApp is RunProvidersCmdInIsolatedAppFull with
// the workspace scope collapsed onto the global config file (dataDir "")
// and RunE errors failing the test.
func RunProvidersCmdInIsolatedApp(t *testing.T, cmd *cobra.Command, providerJSON, args string) (stdout, dataPath string) {
	out, globalPath, _, runErr := RunProvidersCmdInIsolatedAppFull(t, cmd, providerJSON, args, "")
	require.NoError(t, runErr, "command RunE failed; stdout was:\n%s", out)
	return out, globalPath
}
