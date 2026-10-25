// Pinned behavior of writeTargetSlot (ktav_doc.go): when ktavAutoMigrate is
// ON and the slot's legacy JSON exists, that legacy file — including a
// symlinked (even dangling) or unreadable rush.json — stays the write
// target; the Ktav spelling is created only when NEITHER file exists. The
// gate term `!ktavEntryExists(legacyPath)` in writeTargetSlot is load
// bearing: removing it (a) makes writeTargetSlot redirect an existing
// regular rush.json to rush.ktav and fail TestWriteTargetSlotKeepsExistingLegacyJSON
// case (a), and (b) makes the first deferred-symlink workspace write
// materialize a near-empty rush.ktav that shadows rush.json, failing
// TestWriteTargetSlotDeferredSymlinkKeepsLegacyTarget. Hand-trace: T1's
// first assertion is case (a)'s require.Equal(legacy, writeTargetSlot(legacy))
// — with the term removed, selectConfigSlot returns legacy, newConfigFileIsKtav
// is true under the gate, and the function would return ktavSiblingOf(legacy).
// T2's first interesting assertion is require.NoFileExists(dir/rush.ktav)
// after SetConfigFields(ScopeWorkspace, {"options.debug": true}) — with the
// term removed, writeTargetSlot(logical) would return dir/rush.ktav and the
// write would create it. T3's first assertion after LockSettings is the
// sync_rev hash landing inside the symlink target rush.json — same predicate
// keeps the write on the legacy path. The deferred NON-symlink case
// (unreadable-but-regular rush.json) is covered by the migration tests, not
// here. ScopeWorkspace trace (load.go loadOnce): workspacePath is
// normalizeReloadPath(workspaceConfigPath(cfg.Options.DataDirectory))
// (load.go:96-97,105-106) = selectConfigSlot(<dataDir>/rush.json), which
// returns the legacy spelling while rush.json exists, so store_write.go
// configPath (store_write.go:24-29) hands SetConfigFields exactly the
// symlink path and writeTargetSlot keeps it.
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

// TestWriteTargetSlotDeferredSymlinkKeepsLegacyTarget mirrors
// TestKtavLoadDefersSymlinkedLegacyConfig but goes through an actual
// programmatic write: a symlinked rush.json must receive the workspace
// write through the link — no rush.ktav may materialize on either side.
func TestWriteTargetSlotDeferredSymlinkKeepsLegacyTarget(t *testing.T) {
	enableKtavAutoMigrateForTest(t)
	ktavIsolateGlobals(t)
	dir := t.TempDir()
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

	// Load(dir, dir, ...) resolves the workspace slot to dir/rush.json
	// (load.go loadOnce: workspacePath = normalizeReloadPath(
	// workspaceConfigPath(cfg.Options.DataDirectory)); selectConfigSlot
	// returns the legacy spelling while rush.json Lstat-exists), so this
	// write targets the symlink itself and no fallback store is needed.
	require.NoError(t, store.SetConfigFields(ScopeWorkspace, map[string]any{"options.debug": true}))

	require.NoFileExists(t, filepath.Join(dir, "rush.ktav"))
	require.NoFileExists(t, filepath.Join(dir, "dotfiles", "rush.ktav"))
	persisted, err := os.ReadFile(physical)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(persisted, "options.debug").Bool())
	require.Equal(t, "https://example.invalid/v1", gjson.GetBytes(persisted, "providers.custom.base_url").String())
	linkInfo, err := os.Lstat(logical)
	require.NoError(t, err)
	require.NotEqual(t, os.FileMode(0), linkInfo.Mode()&os.ModeSymlink)
	require.NoFileExists(t, migrationRecordPathFor(logical))
	require.NoFileExists(t, backupPathFor(logical))
}

// TestWriteTargetSlotDeferredSymlinkLockAndOAuth pushes a settings lock and
// an OAuth CAS write through a symlinked rush.json global slot: both must
// land in the physical target and leave no Ktav sibling, backup, or
// migration record behind.
func TestWriteTargetSlotDeferredSymlinkLockAndOAuth(t *testing.T) {
	enableKtavAutoMigrateForTest(t)
	ktavIsolateGlobals(t)
	dir := t.TempDir()
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

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("p1", ProviderConfig{ID: "p1", OAuthToken: tokenA})
	store := newTestConfigStore(testStoreOpts{config: &Config{Providers: providers}, globalDataPath: logical})
	t.Cleanup(func() { SetProcessPassword("") })
	trackOAuthJournalTestStore(t, store)

	require.NoError(t, store.LockSettings(ScopeGlobal, "pw"))
	locked, err := os.ReadFile(physical)
	require.NoError(t, err)
	require.Equal(t, HashPassword("pw"), gjson.GetBytes(locked, "sync_rev").String())

	tokenC := &oauth.Token{AccessToken: "access-c", RefreshToken: "refresh-c", ExpiresIn: 3600, ExpiresAt: 9999999999}
	committed, err := store.setOAuthTokenIfCurrent(ScopeGlobal, "p1", tokenA, tokenC)
	require.NoError(t, err)
	require.True(t, committed)
	updated, err := os.ReadFile(physical)
	require.NoError(t, err)
	require.Equal(t, tokenC.RefreshToken, gjson.GetBytes(updated, "providers.p1.oauth.refresh_token").String())

	require.NoFileExists(t, filepath.Join(dir, "rush.ktav"))
	require.NoFileExists(t, filepath.Join(dir, "dotfiles", "rush.ktav"))
	linkInfo, err := os.Lstat(logical)
	require.NoError(t, err)
	require.NotEqual(t, os.FileMode(0), linkInfo.Mode()&os.ModeSymlink)
	require.NoFileExists(t, backupPathFor(logical))
	require.NoFileExists(t, migrationRecordPathFor(logical))
}
