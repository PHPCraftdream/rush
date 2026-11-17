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

func TestGlobalDirUnsetKeepsLegacyOverrides(t *testing.T) {
	base := globalDirIsolate(t)
	require.Empty(t, os.Getenv(GlobalDirEnv))

	require.Equal(t, filepath.Join(base, "env-config", "rush.json"), GlobalConfig())
	require.Equal(t, filepath.Join(base, "env-data", "rush.json"), GlobalConfigData())
	require.Equal(t, filepath.Join(base, "env-cache"), GlobalCacheDir())
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
