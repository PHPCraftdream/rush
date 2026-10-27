// TestMain disables the Ktav auto-migration gate (ktavAutoMigrate) for the
// whole internal/cmd package, restoring it after the run.
//
// WHY: the overwhelming majority of tests in this package are LEGACY
// rush.json fixture tests — they seed a rush.json in an isolated temp data
// dir, run a CLI command, then read rush.json back / assert raw JSON bytes /
// assert the "rush.json" file name. With the gate ON (the production default
// since the Ktav activation), config.Load converts such a seed to rush.ktav
// (rush.json renamed to rush.json.bak), so every one of those assertions
// fails without the config itself being wrong. Disabling the gate here puts
// those tests back in the legacy world they were written for, and each test
// still asserts exactly what it asserted before.
//
// The Ktav-enabled behaviour is covered explicitly by tests that control the
// gate themselves and must not rely on this default:
//   - migrate_ktav_test.go (TestMigrateKtavGateOn* / TestMigrateKtavGateOff*)
//     calls config.SetKtavAutoMigrateForTest(true/false) per test;
//   - lock_ktav_test.go (TestLock_KtavSlotFileNameInOutput) sets the gate
//     explicitly;
//   - ps_stale_ktav_test.go seeds rush.ktav directly via ktavcodec (reading
//     rush.ktav is gate-independent).
//
// If you add a test that exercises the ON default, set the gate true inside
// that test — do not depend on TestMain's value.
package cmd

import (
	"fmt"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/testguard"
)

func TestMain(m *testing.M) {
	// Never let a test reach the operator's real global config.
	cleanup, err := testguard.Isolate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: isolate global state: %v\n", err)
		os.Exit(1)
	}
	restore := config.SetKtavAutoMigrateForTest(false)
	code := m.Run()
	restore()
	cleanup()
	os.Exit(code)
}

// ktavGateOn explicitly enables the Ktav auto-migration gate for one test,
// overriding TestMain's package-wide gate-off default. Use it in tests that
// must exercise the ON (production-default) path; restore is registered via
// t.Cleanup, so never combine with t.Parallel.
func ktavGateOn(t *testing.T) {
	t.Helper()
	restore := config.SetKtavAutoMigrateForTest(true)
	t.Cleanup(restore)
}
