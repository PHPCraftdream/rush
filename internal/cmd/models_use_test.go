// Tests for the worker/reviewer CLI settability gap: `crush models use` gained
// optional --worker/--reviewer flags, `crush models state` reports all four
// slots, and `crush models unset` can clear worker/reviewer individually or
// via the new "all" positional. These tests run the real RunE functions
// against an isolated data dir (same harness as runProvidersCmdInIsolatedApp
// in providers_test.go) so behavior is asserted against actual code paths,
// not a reimplementation.
package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/db"
	crushlog "github.com/charmbracelet/crush/internal/log"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolatedModelsEnv stands up a temp global data dir with a minimal
// crush.json and chdirs into a temp workspace. Unlike
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
func isolatedModelsEnv(t *testing.T) (globalPath string) {
	t.Helper()
	// config.ResetProviderCacheForTests: internal/config's provider/hyper
	// catalog resolution is memoized process-wide via sync.Once — whichever
	// test in this binary calls it FIRST freezes the result (embedded vs.
	// on-disk cache vs. network) for every other test, regardless of their
	// own CRUSH_PROVIDER_CACHE_ONLY/CRUSH_GLOBAL_DATA. Safe here because
	// these tests are serial (no t.Parallel()) — see the exported func's
	// own doc comment for why this must never be called from a parallel
	// test.
	config.ResetProviderCacheForTests()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("CRUSH_GLOBAL_DATA", dataDir)
	// GlobalConfig() (CRUSH_GLOBAL_CONFIG/XDG_CONFIG_HOME) is a SEPARATE
	// resolution path from GlobalConfigData() (CRUSH_GLOBAL_DATA) above —
	// see CLAUDE.md's "two real config paths" caveat. Without this, app.New()
	// (invoked by e.g. TestModelsBump_AllFourRoles) reads the
	// real host ~/.config/crush/crush.json and, if it configures MCP
	// servers, tries to open real network connections to them from
	// inside the test — observed hanging a stress run for 9+ minutes
	// until the 10-minute go test panic-timeout.
	//
	// configDir MUST be a directory distinct from dataDir, not the same tmp
	// reused for both env vars: lookupConfigs (internal/config/load.go)
	// loads BOTH GlobalConfig() and GlobalConfigData() and merges them via
	// go-jsons, which merges (rather than replaces) array-valued fields on
	// collision. Pointing both env vars at the identical "<dir>/crush.json"
	// path made lookupConfigs load and merge that one file with itself —
	// harmless while the seeded config below has no array fields, but a
	// latent bug (doubled array entries, doubled ConfigStore.LoadedPaths()
	// entries) waiting for a future test to add one. globalPath below stays
	// pointed at dataDir (where GlobalConfigData()/ScopeGlobal actually
	// resolves and where modelsUseCmd/modelsBumpCmd actually write); configDir
	// only needs to exist and be isolated from the real host file —
	// lookupConfigs still merges dataDir's crush.json (with the seeded zai
	// key) in regardless of which of the two paths it physically lives under.
	configDir := filepath.Join(tmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("CRUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("CRUSH_PROVIDER_CACHE_ONLY", "1")

	crushlog.Setup("", false)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	workDir := t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))

	globalPath = filepath.Join(dataDir, "crush.json")
	// Seed a synthetic zai api_key from the start rather than starting
	// from a bare "{}". Live model resolution (a.ResolveModel /
	// configureSelectedModels, exercised by e.g. modelsUseCmd's
	// smart/fast positionals and modelsBumpCmd/modelsStateCmd's role
	// reads) drops any provider whose api_key doesn't resolve non-empty —
	// see configureProviders' zai case in internal/config/load.go. Before
	// CRUSH_GLOBAL_CONFIG was isolated above, that requirement was met by
	// accident via a real host ~/.config/crush/crush.json's real zai key
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
		// under test (crushlog.Setup, setupApp, or the command itself)
		// can still leave a file underneath it with pending Windows
		// handle-release lag, same class of race as the SQLite files —
		// observed directly: a TestModelsBump_GLM52FullStepUp failure
		// with the locked path ending in \002 (workDir, not \001/tmp).
		// waitForSQLiteHandleRelease's actual mechanism (retry the real
		// os.RemoveAll) isn't SQLite-specific despite the name, so it
		// applies here unchanged.
		waitForSQLiteHandleRelease(t, workDir)
	})

	for _, cmd := range []*cobra.Command{modelsUseCmd, modelsStateCmd, modelsUnsetCmd} {
		ensureRootFlagStandIns(cmd, tmp)
		cmd.SetContext(ctx)
	}
	return globalPath
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
// (crush.db/-wal/-shm/-journal) via a rename-in-place check. That missed at
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

func TestModelsUse_TwoPositionalRegression(t *testing.T) {
	// The most important test in this file: the existing, established
	// two-positional `crush models use <smart> <fast>` form must behave
	// identically to before the --worker/--reviewer flags were added.
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_6", "glm5_turbo")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, `"smart"`)
	assert.Contains(t, content, `"glm-4.6"`)
	assert.Contains(t, content, `"fast"`)
	assert.Contains(t, content, `"glm-5-turbo"`)
	// No worker/reviewer keys should appear when the flags are omitted.
	assert.NotContains(t, content, `"worker"`)
	assert.NotContains(t, content, `"reviewer"`)
}

// TestModelsUse_FastFlagOnly_LeavesSmartUntouched is the regression test for
// task #249: previously the only way to change the fast slot was the
// two-positional form, which always rewrote smart too — there was no way
// to touch just one of smart/fast. --fast (mirroring the existing
// --worker/--reviewer pattern) must set ONLY the fast slot, leaving smart
// (and worker/reviewer) exactly as they were before the call.
func TestModelsUse_FastFlagOnly_LeavesSmartUntouched(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_6", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsUseFlags(t)
	_, runErr = runModelsCmd(t, modelsUseCmd, "--fast", "glm4_7_flash")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	// Parse the active "models" object specifically rather than grepping raw
	// content: "recent_models" is a separate MRU history that legitimately
	// keeps older values around (that's its entire purpose) alongside the
	// current "models" object, so a raw-content NotContains would wrongly
	// flag that unrelated history array.
	var doc struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))

	assert.Contains(t, string(doc.Models["smart"]), `"glm-4.6"`, "smart must be untouched by a --fast-only call")
	assert.Contains(t, string(doc.Models["fast"]), `"glm-4.7-flash"`, "fast must reflect the new --fast value")
	assert.NotContains(t, string(doc.Models["fast"]), `"glm-5-turbo"`, "the active fast slot must not still be the OLD value")
}

// TestModelsUse_SmartFlagOnly_LeavesFastUntouched is the --smart mirror of
// the test above.
func TestModelsUse_SmartFlagOnly_LeavesFastUntouched(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_6", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsUseFlags(t)
	_, runErr = runModelsCmd(t, modelsUseCmd, "--smart", "glm4_7_flash")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	// See TestModelsUse_FastFlagOnly_LeavesSmartUntouched for why this
	// parses the active "models" object rather than grepping raw content.
	var doc struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))

	assert.Contains(t, string(doc.Models["smart"]), `"glm-4.7-flash"`, "smart must reflect the new --smart value")
	assert.Contains(t, string(doc.Models["fast"]), `"glm-5-turbo"`, "fast must be untouched by a --smart-only call")
	assert.NotContains(t, string(doc.Models["smart"]), `"glm-4.6"`, "the active smart slot must not still be the OLD value")
}

// TestModelsUse_SmartAndFastFlagsTogether covers setting both via flags in
// one call (still without touching worker/reviewer), as distinct from the
// positional form.
func TestModelsUse_SmartAndFastFlagsTogether(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "--smart", "glm4_6", "--fast", "glm5_turbo")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, `"glm-4.6"`)
	assert.Contains(t, content, `"glm-5-turbo"`)
	assert.NotContains(t, content, `"worker"`)
	assert.NotContains(t, content, `"reviewer"`)
}

// TestModelsUse_PositionalAndSmartFlagConflict_Rejected proves the two forms
// (positional <smart> <fast> vs. --smart/--fast flags) cannot be mixed —
// silently preferring one over the other would be worse than refusing.
func TestModelsUse_PositionalAndSmartFlagConflict_Rejected(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_6", "glm5_turbo", "--smart", "glm4_7_flash")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "cannot combine positional")
}

// TestModelsUse_OnePositionalArg_RejectedAsAmbiguous proves a single
// positional arg (neither the old 2-arg form nor the new 0-arg-plus-flags
// form) is rejected with a clear message rather than silently guessing which
// slot it was meant for.
func TestModelsUse_OnePositionalArg_RejectedAsAmbiguous(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_6")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "got 1")
}

// TestModelsUse_NoArgsNoFlags_RejectedAsNoOp proves calling `models use` with
// nothing at all (no positional args, no --smart/--fast/--worker/--reviewer
// flags) fails clearly instead of silently doing nothing.
func TestModelsUse_NoArgsNoFlags_RejectedAsNoOp(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd)
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "nothing to set")
}

// TestModelsUse_WorkerOnlyViaFlags_NoPositionals proves --worker/--reviewer
// alone (no smart/fast at all, positional or flag) still works exactly as
// before — the new smart/fast flags must not have disturbed the existing
// worker/reviewer-only use case.
func TestModelsUse_WorkerOnlyViaFlags_NoPositionals(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_6", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsUseFlags(t)
	_, runErr = runModelsCmd(t, modelsUseCmd, "--worker", "glm4_7_flash")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, `"glm-4.6"`, "large must be untouched")
	assert.Contains(t, content, `"glm-5-turbo"`, "small must be untouched")
	assert.Contains(t, content, `"glm-4.7-flash"`, "worker must reflect the new value")
}

func TestModelsUse_WorkerAndReviewerFlags(t *testing.T) {
	globalPath := isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm4_6", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_3")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, `"glm-4.6"`)       // large
	assert.Contains(t, content, `"glm-5-turbo"`)   // small
	assert.Contains(t, content, `"glm-4.7-flash"`) // worker
	assert.Contains(t, content, `"glm-5.3"`)       // reviewer
}

func TestModelsUse_WorkerViaShortCode(t *testing.T) {
	// Verify the short-code/atom resolution path (o47x, h45l, ...) also
	// applies to the new --worker/--reviewer flags, not just smart/fast.
	globalPath := isolatedModelsEnv(t)

	defer setMockEffortLevels([]string{"low", "medium", "high", "xhigh", "max"})()

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm4_6", "glm5_turbo", "--worker", "fl")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	content := string(data)

	// "fl" is the fable-low short code -> local-cli / cli-claude-fable, effort low.
	assert.Contains(t, content, `"local-cli"`)
	assert.Contains(t, content, `"cli-claude-fable"`)
	assert.Contains(t, content, `"low"`)
}

func TestModelsUse_UnknownWorkerAtomFailsCleanly(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm4_6", "glm5_turbo", "--worker", "not-a-real-atom-xyz")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "worker:")
}

// resetModelsUseFlags clears the persisted flag values on modelsUseCmd so
// state from a previous test in this same process doesn't leak in (cobra
// commands are package-level vars, shared across all tests in this file).
func resetModelsUseFlags(t *testing.T) {
	t.Helper()
	for _, fl := range []string{"global", "local", "smart", "fast", "worker", "reviewer"} {
		if f := modelsUseCmd.Flags().Lookup(fl); f != nil {
			_ = f.Value.Set(f.DefValue)
		}
	}
	modelsUseCmd.SetArgs(nil)
}

func resetModelsUnsetFlags(t *testing.T) {
	t.Helper()
	for _, fl := range []string{"global", "local"} {
		if f := modelsUnsetCmd.Flags().Lookup(fl); f != nil {
			_ = f.Value.Set(f.DefValue)
		}
	}
	modelsUnsetCmd.SetArgs(nil)
}

func resetModelsStateFlags(t *testing.T) {
	t.Helper()
	if f := modelsStateCmd.Flags().Lookup("json"); f != nil {
		_ = f.Value.Set(f.DefValue)
	}
	modelsStateCmd.SetArgs(nil)
}
