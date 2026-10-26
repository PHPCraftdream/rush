// --global-dir (RUSH_GLOBAL_DIR) resolver routing: when the env var is set
// every Rush-owned global path resolves under the single root and all
// other global-path overrides (RUSH_GLOBAL_CONFIG/RUSH_GLOBAL_DATA/
// RUSH_CACHE_DIR/XDG_*) are ignored; when it is unset the historical
// override chain is unchanged.
package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
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
			t.Setenv(GlobalDirEnv, gdRoot)

			require.Equal(t, filepath.Join(gdRoot, "config", "rush.json"), GlobalConfig())
			require.Equal(t, filepath.Join(gdRoot, "data", "rush.json"), GlobalConfigData())
			require.Equal(t, filepath.Join(gdRoot, "cache"), GlobalCacheDir())
			require.Equal(t, filepath.Join(gdRoot, "data"), GlobalWorkspaceDir())
			require.Equal(t, filepath.Join(gdRoot, "data", "catwalk.json"), cachePathFor("catwalk"))
			// canonicalGlobalDataPath routes into the isolated root even
			// under tests (the testing.Testing() stub only applies when
			// --global-dir is NOT active — see settings_lock_sources.go).
			require.Equal(t, filepath.Join(gdRoot, "data", "rush.json"), canonicalGlobalDataPath())
			require.NotEqual(t, base, gdRoot)
		})
	}
}

func TestGlobalDirUnsetKeepsLegacyOverrides(t *testing.T) {
	base := globalDirIsolate(t)
	require.Empty(t, os.Getenv(GlobalDirEnv))

	require.Equal(t, filepath.Join(base, "env-config", "rush.json"), GlobalConfig())
	require.Equal(t, filepath.Join(base, "env-data", "rush.json"), GlobalConfigData())
	require.Equal(t, filepath.Join(base, "env-cache"), GlobalCacheDir())
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
	t.Setenv(GlobalDirEnv, gdRoot)
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

func TestEnsureGlobalDirTree(t *testing.T) {
	globalDirIsolate(t)
	gdRoot := filepath.Join(t.TempDir(), "root")
	t.Setenv(GlobalDirEnv, gdRoot)

	EnsureGlobalDirTree()

	for _, dir := range []string{
		gdRoot,
		filepath.Join(gdRoot, "config"),
		filepath.Join(gdRoot, "data"),
		filepath.Join(gdRoot, "cache"),
	} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		require.True(t, info.IsDir())
		if runtime.GOOS != "windows" {
			require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
		}
	}

	// Without the env var set, EnsureGlobalDirTree is a no-op and creates
	// nothing outside an active root.
	t.Setenv(GlobalDirEnv, "")
	EnsureGlobalDirTree()
}
