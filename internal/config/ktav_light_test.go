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
	ktavLightIsolateEnv(t)
	// NOT parallel: mutates the shared ktavAutoMigrate global.
	require.True(t, ktavAutoMigrate, "KTAV-4: auto-migrate is the default")
	restore := SetKtavAutoMigrateForTest(false)
	require.False(t, ktavAutoMigrate)
	restore()
	require.True(t, ktavAutoMigrate)
}

func TestKtavAutoMigrateEnabled(t *testing.T) {
	ktavLightIsolateEnv(t)
	// NOT parallel: the gate is a shared global; the test mutates it via the
	// test seam rather than assuming a pristine value.
	on := SetKtavAutoMigrateForTest(false)
	require.False(t, KtavAutoMigrateEnabled())
	on()
	require.True(t, KtavAutoMigrateEnabled())
}
