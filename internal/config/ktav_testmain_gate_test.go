// Package-level Ktav auto-migration gate for THIS package's tests.
//
// internal/config contains ~290 legacy-JSON fixture tests that were written
// for the rush.json world: they write rush.json, call config.Load/Init or a
// store write path, and then read rush.json back. Since the KTAV-4
// activation flipped the shipped default ktavAutoMigrate to true
// (ktav_migration.go), Load auto-converts such a fixture to rush.ktav (and
// renames rush.json to rush.json.bak), so those tests would break or hang.
//
// To keep the whole package testable without touching ~290 fixtures, the
// init() below sets the package-level default FOR THIS PACKAGE'S TESTS back
// to false. The real shipped default is captured in ktavShippedDefault
// BEFORE the init() flip and is asserted true by TestKtavAutoMigrateEnabled
// (ktav_light_test.go), so the production default can never be silently
// flipped without a failing test.
//
// Tests that REQUIRE the gate on (Ktav-specific behavior) must opt in
// explicitly by calling enableKtavAutoMigrateForTest(t) as the FIRST
// statement of the test, or by pinning the gate themselves. NEVER call
// enableKtavAutoMigrateForTest from a test that uses t.Parallel: the flag
// is a package-level global shared by every test in the binary.
package config

import "testing"

// ktavShippedDefault captures the shipped (production) value of the Ktav
// auto-migration gate before the test-only init() flips it off. Package
// variables initialize in declaration order within the package, but this
// file deliberately captures the value at its own var-init time, which is
// guaranteed to run before any init() function in the package.
var ktavShippedDefault = ktavAutoMigrate

func init() {
	// Legacy-JSON fixture world for this package's tests only; see the
	// comment block above. The shipped default stays true and is pinned by
	// TestKtavAutoMigrateEnabled via ktavShippedDefault.
	ktavAutoMigrate = false
}

// enableKtavAutoMigrateForTest turns the Ktav auto-migration gate ON for the
// calling test and restores the previous value on cleanup. NEVER use with
// t.Parallel: the flag is package-global.
func enableKtavAutoMigrateForTest(t *testing.T) {
	t.Helper()
	prev := ktavAutoMigrate
	ktavAutoMigrate = true
	t.Cleanup(func() { ktavAutoMigrate = prev })
}
