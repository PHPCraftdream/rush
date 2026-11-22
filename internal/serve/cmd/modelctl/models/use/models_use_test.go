package modeluse

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	rushlog "github.com/PHPCraftdream/rush/internal/kit/diag/log"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	modelatoms "github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/models/atoms"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	cmdkit.UseCommandLoadOptions(modelatoms.CommandLoadOptions)
	cmdtest.Main(m)
}

// isolatedModelsEnv stands up a temp global data dir with a minimal
// rush.json and chdirs into a temp workspace. Unlike
// runProvidersCmdInIsolatedApp in providers_test.go — which invokes RunE with
// a separate "carrier" *cobra.Command standing in for the `cmd` parameter —
// this harness attaches the data-dir/debug flags setupApp needs DIRECTLY onto
// the real subcommand and passes that same command as both receiver and the
// `cmd` argument to RunE. That distinction matters here: models_use.go's
// RunE reads its OWN local flags (--worker/--reviewer) via cmd.Flags(), and
// if `cmd` inside RunE were a stand-in carrier without those flags
// registered, GetString would silently return "" instead of erroring —
// exactly the failure mode this harness exists to avoid. Real cobra
// Execute() always calls RunE(cmd, args) with cmd being the actual
// subcommand instance, so this matches production behavior more closely.
func isolatedModelsEnv(t *testing.T) (globalPath string, command *cobra.Command) {
	t.Helper()
	// config.ResetProviderCacheForTests: internal/config's provider/hyper
	// catalog resolution is memoized process-wide via sync.Once — whichever
	// test in this binary calls it FIRST freezes the result (embedded vs.
	// on-disk cache vs. network) for every other test, regardless of their
	// own RUSH_PROVIDER_CACHE_ONLY/RUSH_GLOBAL_DATA. Safe here because
	// these tests are serial (no t.Parallel()) — see the exported func's
	// own doc comment for why this must never be called from a parallel
	// test.
	config.ResetProviderCacheForTests()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	// GlobalConfig() (RUSH_GLOBAL_CONFIG/XDG_CONFIG_HOME) is a SEPARATE
	// resolution path from GlobalConfigData() (RUSH_GLOBAL_DATA) above —
	// see CLAUDE.md's "two real config paths" caveat. Without this, app.New()
	// (invoked by e.g. TestModelsBump_AllFourRoles) reads the
	// real host ~/.config/rush/rush.json and, if it configures MCP
	// servers, tries to open real network connections to them from
	// inside the test — observed hanging a stress run for 9+ minutes
	// until the 10-minute go test panic-timeout.
	//
	// configDir MUST be a directory distinct from dataDir, not the same tmp
	// reused for both env vars: lookupConfigs (internal/config/load.go)
	// loads BOTH GlobalConfig() and GlobalConfigData() and merges them via
	// go-jsons, which merges (rather than replaces) array-valued fields on
	// collision. Pointing both env vars at the identical "<dir>/rush.json"
	// path made lookupConfigs load and merge that one file with itself —
	// harmless while the seeded config below has no array fields, but a
	// latent bug (doubled array entries, doubled ConfigStore.LoadedPaths()
	// entries) waiting for a future test to add one. globalPath below stays
	// pointed at dataDir (where GlobalConfigData()/ScopeGlobal actually
	// resolves and where testUseCmd/modelsBumpCmd actually write); configDir
	// only needs to exist and be isolated from the real host file —
	// lookupConfigs still merges dataDir's rush.json (with the seeded zai
	// key) in regardless of which of the two paths it physically lives under.
	configDir := filepath.Join(tmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	rushlog.Setup("", false)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	// The owned command constructors are the production command instances under test.
	// Set up the use command here, and the callers supply state/bump instances.
	globalPath = filepath.Join(dataDir, "rush.json")
	// Seed a synthetic zai api_key from the start rather than starting
	// from a bare "{}". Live model resolution (a.ResolveModel /
	// configureSelectedModels, exercised by e.g. testUseCmd's
	// smart/fast positionals and modelsBumpCmd/modelsStateCmd's role
	// reads) drops any provider whose api_key doesn't resolve non-empty —
	// see configureProviders' zai case in internal/config/load.go. Before
	// RUSH_GLOBAL_CONFIG was isolated above, that requirement was met by
	// accident via a real host ~/.config/rush/rush.json's real zai key
	// leaking in; now that the leak is closed, every test using this
	// harness needs its own deterministic key so zai atoms keep resolving
	// instead of silently falling back to a default provider (observed:
	// local-cli/cli-claude-haiku) and corrupting the file these tests
	// read back. seedZAIProvider below performs the identical write for
	// callers that want to do it explicitly/redundantly; both write the
	// same content.
	require.NoError(t, os.WriteFile(globalPath, []byte(`{"providers":{"zai":{"api_key":"test-zai-key"}}}`), 0o644))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		_ = os.Chdir(orig)
		cancel()
		// Release only THIS test's own connection (ref-counted, keyed by
		// tmp) rather than db.ResetPool(), which used to nuke the ENTIRE
		// process-wide connection pool — including any other test's still-
		// open connection to a completely different data dir, if that
		// other test happened to be running concurrently (t.Parallel()) in
		// this same package's test binary. That cross-test interference,
		// not just OS-level handle-release lag, was a real contributor to
		// this package's Windows-only "process cannot access the file"
		// flakiness: one test's cleanup could force-close a sibling's live
		// connection out from under it.
		_ = db.Release(tmp)

		// Root-caused Windows-only flake (testing.go's TempDir
		// RemoveAll cleanup): db.Release() above is synchronous —
		// it calls sql.DB.Close() directly under a mutex, which in
		// turn drives modernc.org/sqlite's conn.Close() ->
		// sqlite3_close_v2() -> a synchronous Win32 CloseHandle on
		// the db/-wal/-shm files (modernc's Windows VFS opens files
		// via raw CreateFileW, no goroutines or runtime.SetFinalizer
		// anywhere in modernc.org/sqlite or modernc.org/libc). There
		// is no Go-controlled handle leak here: by the time this
		// Cleanup runs, the DB was already closed once via
		// db.Release in app.Shutdown's cleanup wg (awaited
		// synchronously by "defer a.Shutdown()" in every models_*
		// RunE before runModelsCmd returns), and ResetPool above is
		// just a belt-and-suspenders no-op repeat of that.
		//
		// The actual failure is the OS/kernel not finishing the
		// handle release before t.TempDir()'s own registered
		// cleanup (which runs AFTER this one — t.Cleanup is LIFO and
		// this closure is registered after both t.TempDir() calls
		// above) does its os.RemoveAll. Go's testing package already
		// retries ERROR_SHARING_VIOLATION/ERROR_ACCESS_DENIED for up
		// to a hardcoded 2s (see testing_windows.go's
		// isWindowsRetryable + removeAll's arbitraryTimeout), which
		// is normally plenty but was observed to be exceeded once
		// under full-parallel-suite (-failfast ./...) system load.
		// Actively probe for the sqlite files becoming removable
		// here, bounded well past that 2s window, so this test's own
		// cleanup absorbs the OS lag instead of racing t.TempDir()'s
		// fixed budget.
		waitForSQLiteHandleRelease(t, tmp)

		// Same treatment for workDir (the second t.TempDir() call
		// above, used as this test's cwd): os.Chdir(orig) already ran
		// at the top of this closure, so workDir is no longer the
		// process's current directory by this point, but a command run
		// under test (rushlog.Setup, setupApp, or the command itself)
		// can still leave a file underneath it with pending Windows
		// handle-release lag, same class of race as the SQLite files —
		// observed directly: a TestModelsBump_GLM52FullStepUp failure
		// with the locked path ending in \002 (workDir, not \001/tmp).
		// waitForSQLiteHandleRelease's actual mechanism (retry the real
		// os.RemoveAll) isn't SQLite-specific despite the name, so it
		// applies here unchanged.
		waitForSQLiteHandleRelease(t, workDir)
	})

	testUseCmd := NewUseCmd()
	parent := &cobra.Command{Use: "models"}
	root := &cobra.Command{Use: "rush"}
	root.AddCommand(parent)
	parent.AddCommand(testUseCmd)
	ensureRootFlagStandIns(testUseCmd, tmp)
	testUseCmd.SetContext(ctx)
	return globalPath, testUseCmd
}

// waitForSQLiteHandleRelease polls (bounded) for dataDir to become fully
// removable, absorbing Windows' OS-level lag in finishing a CloseHandle
// after sqlite3_close_v2() returns — Go's stdlib testing package only
// tolerates ERROR_SHARING_VIOLATION/ERROR_ACCESS_DENIED for a fixed 2s (see
// testing_windows.go's isWindowsRetryable + removeAll's arbitraryTimeout),
// which was repeatedly observed exceeded under real concurrent-test load on
// this machine (both across packages and across this package's own
// t.Parallel() subtests).
//
// A prior version of this helper only probed specific named files
// (rush.db/-wal/-shm/-journal) via a rename-in-place check. That missed at
// least one real failure (TestModelsBump_RoleNotSet_ReportsCleanly) where
// something else under dataDir was still locked — the named-file allowlist
// can't be kept exhaustive as isolatedModelsEnv/setupBumpEnv's callers grow
// (config files, lock files, whatever a future command under test happens
// to create). This version instead retries the REAL os.RemoveAll(dataDir)
// directly, mirroring the equivalent fix in
// internal/agent/cliprovider/provider_test.go's waitForRemovable: if this
// preemptive removal succeeds, t.TempDir()'s own later os.RemoveAll simply
// finds nothing to do (RemoveAll on an already-missing path is a no-op
// success), so doing the real deletion here rather than merely probing is
// safe and catches any locked file regardless of name.
func waitForSQLiteHandleRelease(t *testing.T, dataDir string) {
	t.Helper()

	// 30s, not 10s: observed TestModelsBump_GLM52FullStepUp (which cycles
	// the DB open/close path multiple times, stepping a role through
	// several effort levels in one test) exceed a 10s budget under a cold
	// `go test` cache (every test binary recompiling at once — far heavier
	// transient CPU/IO load than a typical warm pre-push run).
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
		// Give up silently: t.TempDir()'s own cleanup will make one more
		// attempt (with its own 2s retry) and report the real error itself
		// if the handle is truly still held. We don't want to fail the
		// test here for a condition Go's own cleanup already surfaces
		// clearly.
		t.Logf("waitForSQLiteHandleRelease: %s still not removable after budget (%v); proceeding anyway", dataDir, lastErr)
	}
}

// ensureRootFlagStandIns registers debug/data-dir flags directly on cmd if
// not already present (idempotent across the package-level command vars
// reused by multiple tests), and points data-dir at tmp. cwd is intentionally
// left unset here — ResolveCwd falls back to os.Getwd(), which
// isolatedModelsEnv has already chdir'd into workDir, so omitting it is
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

// runModelsCmd parses args onto cmd (caller is responsible for resetting any
// flags left over from a prior call via resetModelsUseFlags/
// resetModelsUnsetFlags — cobra commands are package-level vars, shared
// across all tests in this file), captures stdout, and runs the real RunE
// with cmd as its own receiver (see isolatedModelsEnv for why).
func runModelsCmd(t *testing.T, cmd *cobra.Command, args ...string) (stdout string, runErr error) {
	t.Helper()
	require.NoError(t, cmd.ParseFlags(args))

	var buf bytes.Buffer
	oldOut := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, r); close(done) }()

	runErr = cmd.RunE(cmd, cmd.Flags().Args())

	_ = w.Close()
	os.Stdout = oldOut
	<-done
	return buf.String(), runErr
}

func resetModelsUseFlags(t *testing.T, cmd *cobra.Command) {
	t.Helper()
	for _, name := range []string{"global", "local", "smart", "fast", "worker", "reviewer"} {
		if f := cmd.Flags().Lookup(name); f != nil {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	}
}
