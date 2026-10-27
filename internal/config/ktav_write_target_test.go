// Pinned behavior of writeTargetSlot (ktav_doc.go) when ktavAutoMigrate is
// ON. T1 pins the slot predicate: existing legacy JSON wins and the Ktav
// spelling is created only when NEITHER file exists; the gate term
// `!ktavEntryExists(legacyPath)` in writeTargetSlot is load bearing —
// removing it (a) makes writeTargetSlot redirect an existing regular
// rush.json to rush.ktav and fail TestWriteTargetSlotKeepsExistingLegacyJSON
// case (a). The SYMLINK cases T2/T3 exercise the KTAV-6 behavior: Load
// MIGRATES a symlinked rush.json (rush.ktav published beside the link path,
// link removed, physical target untouched as the backup), and subsequent
// writes land in dir/rush.ktav. Hand-trace: T1's first assertion is case
// (a)'s require.Equal(legacy, writeTargetSlot(legacy)) — with the term
// removed, selectConfigSlot returns legacy, newConfigFileIsKtav is true
// under the gate, and the function would return ktavSiblingOf(legacy). T2's
// first interesting assertion is require.FileExists(dir/rush.ktav) after
// Load — migrateKtavSymlinkedConfig publishes it beside the link path —
// followed by the decoded gjson options.debug check after the write. T3's
// first assertion after LockSettings is the sync_rev hash landing inside
// dir/rush.ktav (decoded via decodeKtavFixtureJSON), since the migrated
// slot is the Ktav file. The deferred NON-symlink case (unreadable-but-
// regular rush.json) is unchanged and covered by the migration tests, not
// here. ScopeWorkspace trace (load.go loadOnce): workspacePath is
// normalizeReloadPath(workspaceConfigPath(cfg.Options.DataDirectory))
// (load.go:96-97,105-106) = selectConfigSlot(<dataDir>/rush.json); after
// the symlink migration rush.json no longer exists, so the slot resolves to
// dir/rush.ktav and store_write.go configPath (store_write.go:24-29) hands
// SetConfigFields exactly the Ktav path.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestWriteTargetSlotKeepsExistingLegacyJSON pins the slot predicate
// directly: existing legacy JSON wins, absent slot redirects to Ktav under
// the gate, and an existing Ktav sibling stays the target.
func TestWriteTargetSlotKeepsExistingLegacyJSON(t *testing.T) {
	enableKtavAutoMigrateForTest(t)
	ktavIsolateGlobals(t)
	dir := t.TempDir()

	// (a) regular rush.json exists, no rush.ktav: the legacy path is kept.
	legacy := filepath.Join(dir, "rush.json")
	writeTestFile(t, legacy, `{"options":{"debug":false}}`)
	require.Equal(t, legacy, writeTargetSlot(legacy))

	// (b) neither file exists: creation decision follows the gate.
	absent := filepath.Join(dir, "other", "rush.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(absent), 0o755))
	require.Equal(t, ktavSiblingOf(absent), writeTargetSlot(absent))

	// (c) rush.ktav exists: the Ktav spelling is the selected target.
	writeTestFile(t, ktavSiblingOf(legacy), `{"options":{"debug":false}}`)
	require.Equal(t, ktavSiblingOf(legacy), writeTargetSlot(legacy))
}

// TestWriteTargetSlotSymlinkMigratedWritesLandInKtav mirrors
// TestKtavLoadMigratesSymlinkedLegacyConfig but follows the migration with
// an actual programmatic write: after Load removes the link and publishes
// dir/rush.ktav, a fresh Load resolves the workspace slot to that Ktav file
// and SetConfigFields lands options.debug there — the physical target is
// never written.
func TestWriteTargetSlotSymlinkMigratedWritesLandInKtav(t *testing.T) {
	enableKtavAutoMigrateForTest(t)
	ktavIsolateGlobals(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	physical := filepath.Join(dir, "dotfiles", "t.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(physical), 0o755))
	original := []byte(`{"providers":{"custom":{"base_url":"https://example.invalid/v1","api_key":"test-key","models":[{"id":"model","name":"Model"}]}},"options":{"disable_default_providers":true}}`)
	require.NoError(t, os.WriteFile(physical, original, 0o600))
	logical := filepath.Join(dir, "rush.json")
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("logical rush.json symlink unavailable: %v", err)
	}

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.NotNil(t, store)

	// Load migrated the symlink: rush.ktav exists beside the link path, the
	// link is kept, and the physical target was never touched.
	ktavPath := ktavSiblingOf(logical)
	require.FileExists(t, ktavPath)
	_, err = os.Lstat(logical)
	require.NoError(t, err)
	persisted, err := os.ReadFile(physical)
	require.NoError(t, err)
	require.Equal(t, original, persisted)
	require.NoFileExists(t, migrationRecordPathFor(logical))
	require.NoFileExists(t, backupPathFor(logical))

	// Perform the write through a fresh Load so the slot capture reflects
	// the migrated state.
	store2, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.NotNil(t, store2)
	require.NoError(t, store2.SetConfigFields(ScopeWorkspace, map[string]any{"options.debug": true}))

	require.NoFileExists(t, filepath.Join(dir, "dotfiles", "rush.ktav"))
	jsonBytes := decodeKtavFixtureJSON(t, ktavPath)
	require.True(t, gjson.GetBytes(jsonBytes, "options.debug").Bool())
	require.Equal(t, "https://example.invalid/v1", gjson.GetBytes(jsonBytes, "providers.custom.base_url").String())
	persisted, err = os.ReadFile(physical)
	require.NoError(t, err)
	require.Equal(t, original, persisted)
}

// TestWriteTargetSlotSymlinkMigratedLockAndOAuth pushes a settings lock and
// an OAuth CAS write through a symlinked rush.json global slot after the
// KTAV-6 migration: both must land in dir/rush.ktav (decoded for the
// assertions) and leave the physical target untouched, with no Ktav sibling
// in dotfiles/, no backup, and no migration record.
func TestWriteTargetSlotSymlinkMigratedLockAndOAuth(t *testing.T) {
	enableKtavAutoMigrateForTest(t)
	ktavIsolateGlobals(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	physical := filepath.Join(dir, "dotfiles", "t.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(physical), 0o755))
	tokenA := &oauth.Token{AccessToken: "access-a", RefreshToken: "refresh-a", ExpiresIn: 3600, ExpiresAt: 9999999999}
	tokenARaw, err := json.Marshal(tokenA)
	require.NoError(t, err)
	original, err := json.Marshal(map[string]any{
		"providers": map[string]any{
			"p1": map[string]any{
				"api_key": "access-a",
				"oauth":   json.RawMessage(tokenARaw),
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(physical, original, 0o600))
	logical := filepath.Join(dir, "rush.json")
	if err := os.Symlink(physical, logical); err != nil {
		t.Skipf("logical rush.json symlink unavailable: %v", err)
	}

	// Run Load first so the KTAV-6 symlink migration happens: the link is
	// kept and rush.ktav is published beside the link path; the store is
	// then built directly on the migrated slot.
	_, err = Load(dir, dir, false)
	require.NoError(t, err)
	ktavPath := ktavSiblingOf(logical)
	require.FileExists(t, ktavPath)
	_, err = os.Lstat(logical)
	require.NoError(t, err)
	persistedOriginal, err := os.ReadFile(physical)
	require.NoError(t, err)
	require.Equal(t, original, persistedOriginal)

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("p1", ProviderConfig{ID: "p1", OAuthToken: tokenA})
	store := newTestConfigStore(testStoreOpts{config: &Config{Providers: providers}, globalDataPath: ktavPath})
	t.Cleanup(func() { SetProcessPassword("") })
	trackOAuthJournalTestStore(t, store)

	require.NoError(t, store.LockSettings(ScopeGlobal, "pw"))
	locked := decodeKtavFixtureJSON(t, ktavPath)
	require.True(t, VerifyPassword("pw", gjson.GetBytes(locked, "sync_rev").String()))

	tokenC := &oauth.Token{AccessToken: "access-c", RefreshToken: "refresh-c", ExpiresIn: 3600, ExpiresAt: 9999999999}
	committed, err := store.setOAuthTokenIfCurrent(ScopeGlobal, "p1", tokenA, tokenC)
	require.NoError(t, err)
	require.True(t, committed)
	updated := decodeKtavFixtureJSON(t, ktavPath)
	require.Equal(t, tokenC.RefreshToken, gjson.GetBytes(updated, "providers.p1.oauth.refresh_token").String())

	require.NoFileExists(t, filepath.Join(dir, "dotfiles", "rush.ktav"))
	persisted, err := os.ReadFile(physical)
	require.NoError(t, err)
	require.Equal(t, original, persisted)
	require.NoFileExists(t, backupPathFor(logical))
	require.NoFileExists(t, migrationRecordPathFor(logical))
}
