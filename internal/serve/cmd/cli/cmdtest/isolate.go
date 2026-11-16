// Package cmdtest holds the test-isolation and fixture helpers shared by
// the split cmd packages (test seam, lane K1; copies of the helpers that
// live in internal/serve/cmd's _test files until lane K3 deletes those).
package cmdtest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
)

// Main is the shared TestMain body: it isolates the operator's real global
// config (testguard.Isolate, as internal/serve/cmd's main_test.go does) and
// applies the whole-binary tweaks settings_lock_kdf_testinit_test.go makes:
// the Ktav auto-migration gate off (legacy rush.json fixture world) and the
// PBKDF2 iteration cost lowered (fixtures only).
func Main(m *testing.M) {
	// Never let a test reach the operator's real global config.
	cleanup, err := testguard.Isolate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: isolate global state: %v\n", err)
		os.Exit(1)
	}
	restore := config.SetKtavAutoMigrateForTest(false)
	_ = config.SetSettingsPasswordIterationsForTest(1000)
	code := m.Run()
	restore()
	cleanup()
	os.Exit(code)
}

// KtavGateOn explicitly enables the Ktav auto-migration gate for one test,
// overriding Main's package-wide gate-off default. Restore is registered via
// t.Cleanup, so never combine with t.Parallel.
func KtavGateOn(t *testing.T) {
	t.Helper()
	restore := config.SetKtavAutoMigrateForTest(true)
	t.Cleanup(restore)
}

// KtavIsolateGlobalEnv isolates every environment variable consulted when
// resolving the global config/data locations. Uses t.Setenv, so tests
// calling this must NOT be t.Parallel, and it must run after (or instead of)
// other t.Setenv-based isolators — the last write wins.
func KtavIsolateGlobalEnv(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("eval symlinks %s: %v", root, err)
	}
	for _, name := range []string{
		"RUSH_GLOBAL_DATA",
		"RUSH_GLOBAL_CONFIG",
		"XDG_CONFIG_HOME",
		"XDG_DATA_HOME",
		"HOME",
		"USERPROFILE",
		"LOCALAPPDATA",
	} {
		t.Setenv(name, filepath.Join(resolved, name))
	}
}
