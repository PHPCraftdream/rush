// --global-dir (RUSH_GLOBAL_DIR) resolver routing: when the env var is set
// every Rush-owned global path resolves under the single root and all
// other global-path overrides (RUSH_GLOBAL_CONFIG/RUSH_GLOBAL_DATA/
// RUSH_CACHE_DIR/XDG_*) are ignored; when it is unset the historical
// override chain is unchanged.
package settings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"
	"github.com/PHPCraftdream/rush/internal/kit/text/ktavcodec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

// globalDirIsolate redirects every env var that could leak into a global
// path resolution to fresh subdirectories of one temp dir, so no test in
// this file can ever read or write the operator's real global state.
func globalDirIsolate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(root, "env-config"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "env-config"))
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(root, "env-data"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "env-data"))
	t.Setenv("RUSH_CACHE_DIR", filepath.Join(root, "env-cache"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "env-cache"))
	return root
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return nil
	}
	require.NoError(t, e)
	return b
}

func TestGlobalDirResolvers(t *testing.T) {
	scenarios := []struct {
		name string
		// alsoOverrides additionally sets RUSH_GLOBAL_CONFIG/RUSH_GLOBAL_DATA/
		// RUSH_CACHE_DIR/XDG_* (done by globalDirIsolate) — they must be ignored.
		alsoOverrides bool
	}{
		{name: "root alone", alsoOverrides: false},
		{name: "root with other overrides ignored", alsoOverrides: true},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			base := globalDirIsolate(t)
			if !scenario.alsoOverrides {
				// Clear the overrides globalDirIsolate installed so this
				// scenario exercises the root alone.
				t.Setenv("RUSH_GLOBAL_CONFIG", "")
				t.Setenv("XDG_CONFIG_HOME", "")
				t.Setenv("RUSH_GLOBAL_DATA", "")
				t.Setenv("XDG_DATA_HOME", "")
				t.Setenv("RUSH_CACHE_DIR", "")
				t.Setenv("XDG_CACHE_HOME", "")
			}
			gdRoot := filepath.Join(t.TempDir(), "root")
			t.Setenv(datadir.GlobalDirEnv, gdRoot)

			require.Equal(t, filepath.Join(gdRoot, "config", "rush.json"), datadir.GlobalConfig())
			require.Equal(t, filepath.Join(gdRoot, "data", "rush.json"), datadir.GlobalConfigData())
			require.Equal(t, filepath.Join(gdRoot, "cache"), datadir.GlobalCacheDir())
			require.Equal(t, filepath.Join(gdRoot, "data"), datadir.GlobalWorkspaceDir())
			require.Equal(t, filepath.Join(gdRoot, "data", "catwalk.json"), datadir.CachePathFor("catwalk"))
			// canonicalGlobalDataPath routes into the isolated root even
			// under tests (the testing.Testing() stub only applies when
			// --global-dir is NOT active — see settings_lock_sources.go).
			require.Equal(t, filepath.Join(gdRoot, "data", "rush.json"), canonicalGlobalDataPath())
			require.NotEqual(t, base, gdRoot)
		})
	}
}

// TestGlobalDirLockRouting verifies the settings-lock isolation contract at
// the level the production code implements it (canonicalGlobalDataPath +
// readSettingsHash): under --global-dir the canonical lock source is the
// file inside <root>/data, so a lock living only on the machine's real
// (env-overridden) data path is invisible, while a lock inside
// <root>/data is enforced.
func TestGlobalDirLockRouting(t *testing.T) {
	base := globalDirIsolate(t)
	gdRoot := filepath.Join(t.TempDir(), "root")
	t.Setenv(datadir.GlobalDirEnv, gdRoot)
	withProcessPassword(t, "wrong-password")

	canonical := canonicalGlobalDataPath()
	require.Equal(t, filepath.Join(gdRoot, "data", "rush.json"), canonical)

	// A lock on the env-overridden (non-isolated) data path does NOT block:
	// the canonical source points into the root, not there.
	lockedDoc := writeLockedKtavDoc(t, filepath.Join(base, "env-data", "rush.ktav"), "other-password", "")
	hash, err := readSettingsHash(canonical, filepath.Join(gdRoot, "data", "other.json"), lockedDoc)
	require.NoError(t, err)
	require.Empty(t, hash, "lock outside the isolated root must be invisible")

	// A lock inside <root>/data DOES block: readSettingsHash surfaces its
	// hash, which VerifyPassword rejects for a non-holder password.
	writeLockedKtavDoc(t, filepath.Join(gdRoot, "data", "rush.ktav"), "holder-password", "")
	hash, err = readSettingsHash(canonical, filepath.Join(gdRoot, "data", "other.json"), lockedDoc)
	require.NoError(t, err)
	require.NotEmpty(t, hash)
	require.False(t, VerifyPassword("wrong-password", hash), "a lock inside the isolated root must be enforced")
}

// integrityModelsFixture returns an offline, deterministic rush.json body.
// modelsJSON may be "" to omit the models object entirely.
func integrityModelsFixture(t *testing.T, dir string, modelsJSON string) string {
	t.Helper()
	doc := `{"options":{"disable_default_providers":true}`
	if modelsJSON != "" {
		doc += `,"models":` + modelsJSON
	}
	doc += `,"providers":{"openai":{"api_key":"test-key","base_url":"https://example.invalid/v1","models":[{"id":"gpt-4","name":"GPT-4"}]}}}`
	return doc
}

func testKtavBytes(t *testing.T, jsonSrc string) []byte {
	t.Helper()
	ktav, err := ktavcodec.ConvertJSONToKtav([]byte(jsonSrc))
	require.NoError(t, err)
	return ktav
}

func mustDecodeTestKtav(t *testing.T, raw []byte) []byte {
	t.Helper()
	tree, err := ktavcodec.DecodeKtav(raw)
	require.NoError(t, err)
	jsonBytes, err := ktavcodec.ToJSON(tree)
	require.NoError(t, err)
	return jsonBytes
}

// ktavIntegrityWrite writes jsonDoc to path as a rush.ktav document and
// returns the decoded JSON bytes of what is on disk.
func ktavIntegrityWrite(t *testing.T, path, jsonDoc string) []byte {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, testKtavBytes(t, jsonDoc), 0o600))
	return mustDecodeTestKtav(t, fileBytes(t, path))
}

// writeLockedKtavDoc writes an offline fixture document carrying a sync_rev
// lock to path as a rush.ktav file and returns the decoded JSON bytes.
func writeLockedKtavDoc(t *testing.T, path, password, modelsJSON string) []byte {
	t.Helper()
	doc := integrityModelsFixture(t, path, modelsJSON)
	locked, err := sjson.Set(doc, syncRevKey, HashPassword(password))
	require.NoError(t, err)
	return ktavIntegrityWrite(t, path, locked)
}

// withProcessPassword installs a process password for the test's duration.
func withProcessPassword(t *testing.T, password string) {
	t.Helper()
	SetProcessPassword(password)
	t.Cleanup(func() { SetProcessPassword("") })
}
