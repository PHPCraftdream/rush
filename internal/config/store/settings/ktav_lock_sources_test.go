// KTAV-3a round 2: settings lock-source logic exercised through the settings
// package's own protection entry points. The test here was split from the
// root package's ktav_lock_sources_test.go: it needs ONLY settings privates
// (the canonical source via the SetCanonicalGlobalDataPathForTest seam) and
// no root ConfigStore integration.
package settings

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// lockSourceIsolate redirects EVERY environment variable a global config or
// home-directory resolution can consult (RUSH_GLOBAL_CONFIG, XDG_CONFIG_HOME,
// RUSH_GLOBAL_DATA, XDG_DATA_HOME, HOME, USERPROFILE) to fresh subdirectories
// of one temp dir, so no test in this file can ever read or write the
// operator's real config or real home directory. Returns the temp root; the
// global data dir is root/global-data.
func lockSourceIsolate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(root, "global-config"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "global-config"))
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(root, "global-data"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "global-data"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("USERPROFILE", filepath.Join(root, "home"))
	return root
}

// canonicalTempDir returns a symlink-resolved temp dir: discovery output and
// test-seeded paths must meet in the same spelling on every OS.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

// TestKtavLockSources_CanonicalLegacyStub pins that a lock in the Ktav
// SIBLING of a LOGICAL (legacy-spelled) canonical global path is honored by
// SettingsWriteAllowed. canonicalGlobalDataPath is stubbed to return the
// legacy rush.json spelling on purpose: the selection must happen in the
// read layer, not in the stub.
func TestKtavLockSources_CanonicalLegacyStub(t *testing.T) {
	lockSourceIsolate(t)
	canonicalDir := canonicalTempDir(t)
	writeLockedKtavDoc(t, filepath.Join(canonicalDir, "rush.ktav"), "operator-pw",
		`{"smart":{"provider":"p","model":"m"}}`)

	SetCanonicalGlobalDataPathForTest(t, filepath.Join(canonicalDir, "rush.json"))

	withProcessPassword(t, "wrong-pw")
	require.ErrorIs(t, SettingsWriteAllowed(canonicalTempDir(t)), ErrSettingsLocked,
		"a canonical rush.ktav lock must refuse a settings write")
}
