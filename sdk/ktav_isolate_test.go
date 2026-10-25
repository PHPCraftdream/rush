package sdk_test

// KTAV-4b SDK chunk: shared global-env isolation for Ktav tests. Every
// Ktav test must redirect ALL environment variables the config/session
// layer consults for global state (XDG and non-XDG, Windows and Unix)
// into fresh per-test temp dirs, so no test can read or mutate the real
// user's configuration. (The pre-existing isolateGlobalConfigForWorkdir
// helper in sdk_workdir_test.go covers a smaller set and is left
// untouched; Ktav tests use this stricter helper.)

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// ktavIsolateSDKGlobalEnv points RUSH_GLOBAL_DATA, RUSH_GLOBAL_CONFIG,
// XDG_CONFIG_HOME, XDG_DATA_HOME, HOME, and USERPROFILE at fresh temp
// dirs for the duration of the test.
func ktavIsolateSDKGlobalEnv(t *testing.T) {
	t.Helper()

	// Canonical temp dir (Windows: symlink/8.3-short-name resolved).
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	dataDir := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	configDir := filepath.Join(root, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))

	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
}
