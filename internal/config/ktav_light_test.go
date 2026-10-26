package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/require"
)

// ktavLightSlotDir creates an isolated slot directory and returns the legacy
// JSON path of the rush slot anchored inside it. The helpers under test take
// explicit paths, so this helper is not itself an env isolator; tests call
// ktavLightIsolateEnv(t) at the top of every test body (t.Setenv forbids
// t.Parallel).
func ktavLightSlotDir(t *testing.T) (dir, legacy string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir, filepath.Join(dir, "rush.json")
}

func TestReadConfigDocumentLightJSONOnly(t *testing.T) {
	ktavLightIsolateEnv(t)
	_, legacy := ktavLightSlotDir(t)
	jsonDoc := []byte(`{"models":{"smart":{"provider":"p","model":"m"}}}`)
	require.NoError(t, os.WriteFile(legacy, jsonDoc, 0o644))

	got, err := ReadConfigDocumentLight(legacy)
	require.NoError(t, err)
	require.JSONEq(t, string(jsonDoc), string(got))
}

func TestReadConfigDocumentLightKtavOnly(t *testing.T) {
	ktavLightIsolateEnv(t)
	_, legacy := ktavLightSlotDir(t)
	jsonDoc := []byte(`{"models":{"fast":{"provider":"p","model":"m"}}}`)
	ktavDoc, err := ktavcodec.ConvertJSONToKtav(jsonDoc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ktavSiblingOf(legacy), ktavDoc, 0o644))

	got, err := ReadConfigDocumentLight(legacy)
	require.NoError(t, err)
	var want, have map[string]any
	require.NoError(t, json.Unmarshal(jsonDoc, &want))
	require.NoError(t, json.Unmarshal(got, &have))
	require.Equal(t, want, have)
}

func TestReadConfigDocumentLightBothPresentKtavWins(t *testing.T) {
	ktavLightIsolateEnv(t)
	_, legacy := ktavLightSlotDir(t)
	require.NoError(t, os.WriteFile(legacy, []byte(`{"side":"json"}`), 0o644))
	ktavDoc, err := ktavcodec.ConvertJSONToKtav([]byte(`{"side":"ktav"}`))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ktavSiblingOf(legacy), ktavDoc, 0o644))

	got, err := ReadConfigDocumentLight(legacy)
	require.NoError(t, err)
	var have map[string]any
	require.NoError(t, json.Unmarshal(got, &have))
	require.Equal(t, map[string]any{"side": "ktav"}, have)
}

func TestReadConfigDocumentLightBackupOnlyFailsClosed(t *testing.T) {
	ktavLightIsolateEnv(t)
	_, legacy := ktavLightSlotDir(t)
	require.NoError(t, os.WriteFile(legacy+".bak", []byte(`{"side":"json"}`), 0o644))

	_, err := ReadConfigDocumentLight(legacy)
	require.ErrorIs(t, err, errKtavBackupOnly)
}

func TestReadConfigDocumentLightCorruptKtav(t *testing.T) {
	ktavLightIsolateEnv(t)
	_, legacy := ktavLightSlotDir(t)
	// A duplicated key is definitively invalid Ktav (plain "k: v" lines are
	// valid); decoding must fail rather than yield a document.
	require.NoError(t, os.WriteFile(ktavSiblingOf(legacy), []byte("k: a\nk: b\n"), 0o644))

	_, err := ReadConfigDocumentLight(legacy)
	require.Error(t, err)
}

func TestReadConfigDocumentLightMissingFile(t *testing.T) {
	ktavLightIsolateEnv(t)
	_, legacy := ktavLightSlotDir(t)

	_, err := ReadConfigDocumentLight(legacy)
	require.Error(t, err)
	require.True(t, os.IsNotExist(err), "expected not-exist, got %v", err)
	require.False(t, errors.Is(err, errKtavBackupOnly))
}

func TestActiveConfigFileName(t *testing.T) {
	ktavLightIsolateEnv(t)
	dir, legacy := ktavLightSlotDir(t)

	require.Equal(t, legacy, ActiveConfigFileName(legacy))

	require.NoError(t, os.WriteFile(ktavSiblingOf(legacy), []byte("k: v\n"), 0o644))
	require.Equal(t, filepath.Join(dir, "rush.ktav"), ActiveConfigFileName(legacy))

	require.Empty(t, ActiveConfigFileName(""))
}

func TestSetKtavAutoMigrateForTest(t *testing.T) {
	// This test exercises the gate-ON world (it asserts the ambient value is
	// true before flipping), so it opts in explicitly as its first
	// statement: the package test init() defaults the gate to false. NOT
	// parallel: the helper mutates the shared ktavAutoMigrate global.
	enableKtavAutoMigrateForTest(t)
	ktavLightIsolateEnv(t)
	require.True(t, ktavAutoMigrate, "KTAV-4: auto-migrate is the default")
	restore := SetKtavAutoMigrateForTest(false)
	require.False(t, ktavAutoMigrate)
	restore()
	require.True(t, ktavAutoMigrate)
}

func TestKtavAutoMigrateEnabled(t *testing.T) {
	ktavLightIsolateEnv(t)
	// NOT parallel: the gate is a shared global; the test mutates it via the
	// test seam rather than assuming a pristine value. The SHIPPED default is
	// asserted via ktavShippedDefault (captured before the package test
	// init() flips the gate off), so the production default can never be
	// silently changed without this test going red.
	require.True(t, ktavShippedDefault, "KTAV-4: the shipped default must keep auto-migrate ON")
	// ktavShippedDefault is the var-init capture of ktavAutoMigrate
	// (ktav_migration.go declares it true) taken BEFORE any init() in the
	// package runs, so a true value here pins the declared production
	// default. The AMBIENT value in this test binary is false (the test-only
	// init() flips it), so the seam-restore round-trip must be asserted
	// against the ambient value, not against the shipped default.
	ambient := KtavAutoMigrateEnabled()
	on := SetKtavAutoMigrateForTest(false)
	require.False(t, KtavAutoMigrateEnabled())
	on()
	require.Equal(t, ambient, KtavAutoMigrateEnabled(),
		"restoring the gate must return the ambient value it captured")
}
