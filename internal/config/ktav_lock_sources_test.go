// KTAV-3a round 2: slot-aware settings-protection sources, exercised through
// the PRODUCTION entry points. Every test here drives a real protection API
// (LockState, SettingsWriteAllowed, CheckPasswordAgainstDisk,
// anySettingsLockExists, naturalWorkspaceSettingsPath) against a global or
// workspace slot that holds ONLY a rush.ktav document (or only a retained
// backup), with the store's logical LEGACY path — never a hand-selected
// .ktav path. The slot selection and the backup-only fail-closed rule live
// inside readSettingsDocument (ktav_doc.go), so any reverted selection layer
// fails these tests.
//
// Revert-checks:
//   - dropping the slot selection from readSettingsDocument (reading the
//     literal legacy name) makes every rush.ktav-only lock in this file
//     invisible: LockState/SettingsWriteAllowed report unlocked,
//     CheckPasswordAgainstDisk returns nil for a wrong password.
//   - dropping checkBackupOnlySlot from readSettingsDocument makes the
//     backup-only test read as "no lock" (CheckPasswordAgainstDisk nil,
//     LockState no error, SettingsWriteAllowed nil with any process state).
package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
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

// canonicalTempDir returns a symlink-resolved temp dir: discovery output and
// test-seeded paths must meet in the same spelling on every OS.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

// TestKtavLockSources_GlobalLegacyPathSeesKtavLock pins that a global slot
// created ONLY as rush.ktav locks a store whose globalDataPath is the literal
// legacy rush.json path: the selection layer inside readSettingsDocument —
// not the caller — must pick the Ktav sibling.
func TestKtavLockSources_GlobalLegacyPathSeesKtavLock(t *testing.T) {
	root := lockSourceIsolate(t)
	dataDir := canonicalTempDir(t)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	writeLockedKtavDoc(t, filepath.Join(dataDir, "rush.ktav"), "pw",
		`{"smart":{"provider":"p","model":"m"}}`)
	// Deliberately NO rush.json next to it.

	legacy := GlobalConfigData()
	require.Equal(t, filepath.Join(dataDir, "rush.json"), legacy)
	s := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: legacy})
	s.workingDir = filepath.Join(root, "workspace")

	state, err := s.LockState()
	require.NoError(t, err)
	require.True(t, state.Global, "a lock only in the global rush.ktav must be Global")
	require.True(t, s.anySettingsLockExists())

	withProcessPassword(t, "wrong")
	require.ErrorIs(t, SettingsWriteAllowed(s.workingDir), ErrSettingsLocked,
		"a wrong process password must be refused against a rush.ktav-only global slot")
	require.ErrorIs(t, CheckPasswordAgainstDisk("wrong"), ErrWrongPassword,
		"a rush.ktav-only global lock must be SEEN by CheckPasswordAgainstDisk")
	withProcessPassword(t, "pw")
	require.NoError(t, CheckPasswordAgainstDisk("pw"))
	require.NoError(t, SettingsWriteAllowed(s.workingDir))
}

// TestKtavLockSources_WorkspaceNaturalSource pins that a lock living only in
// the workspace's natural <work>/.rush/rush.ktav document locks writes via
// naturalWorkspaceSettingsPath, which must select the Ktav sibling of the
// legacy name.
func TestKtavLockSources_WorkspaceNaturalSource(t *testing.T) {
	lockSourceIsolate(t)
	work := canonicalTempDir(t)
	ktavPath := filepath.Join(work, ".rush", "rush.ktav")
	writeLockedKtavDoc(t, ktavPath, "pw", `{"smart":{"provider":"p","model":"m"}}`)

	require.Equal(t, ktavPath, naturalWorkspaceSettingsPath(work),
		"the natural workspace source must resolve to the rush.ktav sibling")

	s := newTestConfigStore(testStoreOpts{config: &Config{}})
	s.workingDir = work

	withProcessPassword(t, "wrong")
	require.ErrorIs(t, SettingsWriteAllowed(work), ErrSettingsLocked,
		"a workspace rush.ktav lock must refuse a settings write")
	withProcessPassword(t, "pw")
	require.NoError(t, SettingsWriteAllowed(work))
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

	previous := canonicalGlobalDataPath
	canonicalGlobalDataPath = func() string { return filepath.Join(canonicalDir, "rush.json") }
	t.Cleanup(func() { canonicalGlobalDataPath = previous })

	withProcessPassword(t, "wrong-pw")
	require.ErrorIs(t, SettingsWriteAllowed(canonicalTempDir(t)), ErrSettingsLocked,
		"a canonical rush.ktav lock must refuse a settings write")
}

// TestKtavLockSources_CorruptKtavBehindLegacyPath pins that an UNDECODABLE
// rush.ktav behind a store's LOGICAL legacy path fails closed everywhere:
// SettingsWriteAllowed refuses and anySettingsLockExists reports true.
func TestKtavLockSources_CorruptKtavBehindLegacyPath(t *testing.T) {
	root := lockSourceIsolate(t)
	dataDir := canonicalTempDir(t)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.ktav"),
		[]byte("k: not a ktav document\nk: not a ktav document\n"), 0o600))

	s := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: GlobalConfigData()})
	s.workingDir = filepath.Join(root, "workspace")

	withProcessPassword(t, "pw")
	require.ErrorIs(t, SettingsWriteAllowed(s.workingDir), ErrSettingsLocked,
		"an undecodable protection source must fail closed")
	require.True(t, s.anySettingsLockExists())
}

// TestKtavLockSources_BackupOnlyFailsClosed pins that a global slot holding
// ONLY a retained backup is an ERROR for every protection entry point —
// never "no lock" and never "locked but unreadable is fine".
func TestKtavLockSources_BackupOnlyFailsClosed(t *testing.T) {
	root := lockSourceIsolate(t)
	dataDir := canonicalTempDir(t)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json.bak"),
		[]byte(`{"anything":true}`), 0o600))

	s := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: GlobalConfigData()})
	s.workingDir = filepath.Join(root, "workspace")

	require.Error(t, CheckPasswordAgainstDisk("pw"),
		"a backup-only slot must never authenticate as unlocked")
	require.ErrorIs(t, CheckPasswordAgainstDisk("pw"), ErrSettingsLocked)
	withProcessPassword(t, "pw")
	require.ErrorIs(t, SettingsWriteAllowed(s.workingDir), ErrSettingsLocked)
	_, err := s.LockState()
	require.Error(t, err)
}
