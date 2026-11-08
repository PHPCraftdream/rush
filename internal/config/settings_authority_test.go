// Trusted-UI write-authority tests: the per-call
// SettingsAuthorityTrustedUI parameter lifts ONLY the settings-lock veto,
// never reserved-key protection, model-integrity signing, scope ownership,
// or audit labeling. Every assertion here observes REAL outcomes: on-disk
// bytes, in-memory snapshots, returned errors, and audit events.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const trustedUIFixturePassword = "pw"

// cheapPasswordIterationsForTest keeps PBKDF2 fast for tests that call
// LockSettings/VerifyPassword; restores the previous value via defer.
func cheapPasswordIterationsForTest() (restore func()) {
	return SetSettingsPasswordIterationsForTest(1000)
}

// isolatedLegacyStore is the legacy-JSON fixture used by the trusted-UI
// write tests: fully isolated global config paths, a fresh legacy rush.json
// slot, and the Ktav auto-migration gate pinned off so every disk assertion
// reads raw rush.json bytes.
func isolatedLegacyStore(t *testing.T, name string) (*ConfigStore, string) {
	t.Helper()
	pinKtavAutoMigrateOffForLegacyTest(t)
	_, _ = isolateAllGlobalConfigPaths(t)
	p := filepath.Join(t.TempDir(), name)
	s := lockStore(t, p)
	return s, p
}

// isolatedLoadedStore is the real-Load variant of the trusted-UI fixture: it
// seeds a full custom provider on the isolated global data slot and loads the
// store with config.Load, so post-write autoReload genuinely runs and the
// published snapshot can be asserted. lockStore has no working directory,
// which makes autoReloadAfterWrite a no-op and leaves Providers nil, so the
// memory-publish tests must use this fixture instead.
func isolatedLoadedStore(t *testing.T) (*ConfigStore, string) {
	t.Helper()
	pinKtavAutoMigrateOffForLegacyTest(t)
	_, dataDir := isolateAllGlobalConfigPaths(t)
	p := filepath.Join(dataDir, "rush.json")
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	seed := `{"providers":{"acme":{"id":"acme","name":"acme","type":"openai-compat","base_url":"http://acme.example","api_key":"old","discover_models":false,"models":[{"id":"m1","name":"m1","context_window":1000,"default_max_tokens":500}]}}}`
	require.NoError(t, os.WriteFile(p, []byte(seed), 0o600))
	resetProviderState()
	t.Cleanup(resetProviderState)
	store, err := Load(t.TempDir(), dataDir, false)
	require.NoError(t, err)
	SetProcessPassword("")
	t.Cleanup(func() { SetProcessPassword("") })
	return store, p
}

func requireSyncRevUnchanged(t *testing.T, before []byte, after []byte) {
	t.Helper()
	beforeRev := gjson.GetBytes(before, "sync_rev").String()
	afterRev := gjson.GetBytes(after, "sync_rev").String()
	if afterRev != beforeRev {
		t.Logf("sync_rev changed on disk: before=%q after=%q", beforeRev, afterRev)
	}
	require.Equal(t, beforeRev, afterRev)
}

// Defect caught: a trusted-UI write must apply while the global settings
// lock is active, and the lock itself (sync_rev) must survive the write
// byte-identical and still verify against the lock password.
func TestTrustedUIWriteAppliesUnderGlobalLock(t *testing.T) {
	restore := cheapPasswordIterationsForTest()
	defer restore()
	s, p := isolatedLoadedStore(t)
	require.NoError(t, s.LockSettings(ScopeGlobal, trustedUIFixturePassword))
	lockedBytes := fileBytes(t, p)

	err := s.SetConfigField(ScopeGlobal, "providers.acme.api_key", "refused")
	require.ErrorIs(t, err, ErrSettingsLocked)
	require.Equal(t, lockedBytes, fileBytes(t, p))

	require.NoError(t, s.SetConfigFieldWithAuthority(ScopeGlobal, "providers.acme.api_key", "new", SettingsAuthorityTrustedUI))
	afterBytes := fileBytes(t, p)
	requireSyncRevUnchanged(t, lockedBytes, afterBytes)
	require.True(t, VerifyPassword(trustedUIFixturePassword, gjsonString(afterBytes, "sync_rev")))
	require.Equal(t, "new", gjsonString(afterBytes, "providers.acme.api_key"))
	got, ok := s.Config().Providers.Get("acme")
	require.True(t, ok, "autoReload must publish the trusted write into memory")
	require.Equal(t, "new", got.APIKey)
}

// Defect caught: the trusted-UI authority must NOT lift reserved-key
// protection — sync_rev and cache_rev writes and sync_rev removal stay
// blocked and leave the disk untouched.
func TestTrustedUIWriteCannotTouchReservedKeys(t *testing.T) {
	restore := cheapPasswordIterationsForTest()
	defer restore()
	s, p := isolatedLegacyStore(t, "global.json")
	require.NoError(t, s.SetConfigField(ScopeGlobal, "seed", 1))
	require.NoError(t, s.LockSettings(ScopeGlobal, trustedUIFixturePassword))
	before := fileBytes(t, p)

	errSetSync := s.SetConfigFieldWithAuthority(ScopeGlobal, "sync_rev", map[string]any{}, SettingsAuthorityTrustedUI)
	require.ErrorIs(t, errSetSync, errSettingsLockKeyReserved)
	errSetCache := s.SetConfigFieldWithAuthority(ScopeGlobal, "cache_rev", map[string]any{}, SettingsAuthorityTrustedUI)
	require.ErrorIs(t, errSetCache, errSettingsLockKeyReserved)
	errRemoveSync := s.RemoveConfigFieldWithAuthority(ScopeGlobal, "sync_rev", SettingsAuthorityTrustedUI)
	require.ErrorIs(t, errRemoveSync, errSettingsLockKeyReserved)

	require.Equal(t, before, fileBytes(t, p))
	// The reserved sentinel is an alias of ErrSettingsLocked; the specific
	// sentinel the guard actually returns is errSettingsLockKeyReserved.
	require.ErrorIs(t, errSetSync, ErrSettingsLocked)
}

// Defect caught: a trusted-UI removal must apply under the lock, delete the
// seeded key from disk, and leave sync_rev intact.
func TestTrustedUIRemoveAppliesUnderLock(t *testing.T) {
	restore := cheapPasswordIterationsForTest()
	defer restore()
	s, p := isolatedLoadedStore(t)
	require.NoError(t, s.LockSettings(ScopeGlobal, trustedUIFixturePassword))
	lockedBytes := fileBytes(t, p)

	err := s.RemoveConfigField(ScopeGlobal, "providers.acme.api_key")
	require.ErrorIs(t, err, ErrSettingsLocked)
	require.Equal(t, lockedBytes, fileBytes(t, p))

	require.NoError(t, s.RemoveConfigFieldWithAuthority(ScopeGlobal, "providers.acme.api_key", SettingsAuthorityTrustedUI))
	afterBytes := fileBytes(t, p)
	requireSyncRevUnchanged(t, lockedBytes, afterBytes)
	require.False(t, gjson.GetBytes(afterBytes, "providers.acme.api_key").Exists())
	// RemoveConfigField removes only the key: reload keeps the provider
	// itself because a missing API key is a warning, not a drop.
	got, ok := s.Config().Providers.Get("acme")
	require.True(t, ok, "removing the key must not drop the provider itself")
	require.Empty(t, got.APIKey, "the provider must remain with an empty key")
}

// Defect caught: a trusted-UI models write under the lock must keep the
// models-integrity signature valid (cache_rev refreshed) and sync_rev
// unchanged.
func TestTrustedUIModelsWriteKeepsIntegritySignature(t *testing.T) {
	restore := cheapPasswordIterationsForTest()
	defer restore()
	s, p := isolatedLegacyStore(t, "global.json")
	require.NoError(t, s.LockSettings(ScopeGlobal, trustedUIFixturePassword))
	lockedBytes := fileBytes(t, p)

	err := s.UpdatePreferredModels(ScopeGlobal, map[SelectedModelType]SelectedModel{SelectedModelTypeSmart: {Provider: "x", Model: "y"}})
	require.ErrorIs(t, err, ErrSettingsLocked)
	require.Equal(t, lockedBytes, fileBytes(t, p))

	require.NoError(t, s.UpdatePreferredModelsWithAuthority(ScopeGlobal, map[SelectedModelType]SelectedModel{SelectedModelTypeSmart: {Provider: "zai", Model: "glm-5"}}, SettingsAuthorityTrustedUI))
	afterBytes := fileBytes(t, p)
	requireSyncRevUnchanged(t, lockedBytes, afterBytes)
	require.Equal(t, "zai", gjsonString(afterBytes, "models.smart.provider"))
	require.Equal(t, "glm-5", gjsonString(afterBytes, "models.smart.model"))
	require.NoError(t, modelsIntegrityOf(afterBytes, true), "cache_rev must be a valid signature while a lock exists")
	// No memory assertion here: reload recomputes the effective model
	// defaults from the provider catalog, so the in-memory selection may
	// legitimately differ from the written value. The durable contract is
	// the on-disk document plus its valid cache_rev signature.
}

// Defect caught: the trusted-UI authority lifts the settings-lock veto for
// the matching SCOPE only — a local workspace lock still vetoes guarded
// workspace writes, while trusted workspace writes through that same lock
// apply without touching the workspace's own sync_rev.
func TestTrustedUILiftsWorkspaceLockNotScopeOwnership(t *testing.T) {
	restore := cheapPasswordIterationsForTest()
	defer restore()
	pinKtavAutoMigrateOffForLegacyTest(t)
	_, _ = isolateAllGlobalConfigPaths(t)
	root := t.TempDir()
	global := filepath.Join(root, "global.json")
	workspace := filepath.Join(root, "workspace", ".rush", "rush.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(workspace), 0o700))
	require.NoError(t, os.WriteFile(workspace, []byte(`{"sync_rev":"`+HashPassword("local")+`"}`), 0o600))
	store := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: global, workspacePath: workspace})
	store.workingDir = filepath.Dir(filepath.Dir(filepath.Dir(workspace)))
	SetProcessPassword("")
	t.Cleanup(func() { SetProcessPassword("") })
	before := fileBytes(t, workspace)

	err := store.SetConfigField(ScopeWorkspace, "ui.visibility", "refused")
	require.ErrorIs(t, err, ErrSettingsLocked)
	require.Equal(t, before, fileBytes(t, workspace))

	require.NoError(t, store.SetConfigFieldWithAuthority(ScopeWorkspace, "ui.visibility", "shown", SettingsAuthorityTrustedUI))
	afterBytes := fileBytes(t, workspace)
	require.Equal(t, "shown", gjsonString(afterBytes, "ui.visibility"))
	require.True(t, VerifyPassword("local", gjsonString(afterBytes, "sync_rev")), "the workspace lock hash must survive the trusted write")
}

// Defect caught: under an active lock, concurrent guarded writes stay
// refused (no lost veto, no partial application) while concurrent trusted
// writes all land atomically, with sync_rev preserved across all of them.
func TestConcurrentGuardedWritesStayRefusedWhileTrustedWritesLand(t *testing.T) {
	restore := cheapPasswordIterationsForTest()
	defer restore()
	s, p := isolatedLegacyStore(t, "global.json")
	require.NoError(t, s.SetConfigField(ScopeGlobal, "seed", 1))
	require.NoError(t, s.LockSettings(ScopeGlobal, trustedUIFixturePassword))
	lockedBytes := fileBytes(t, p)

	var auditMu sync.Mutex
	var auditEvents []SettingsEvent
	audits := make(chan struct{}, 32)
	SetSettingsAuditSink(func(e SettingsEvent) {
		auditMu.Lock()
		auditEvents = append(auditEvents, e)
		auditMu.Unlock()
		audits <- struct{}{}
	})
	t.Cleanup(func() { SetSettingsAuditSink(nil) })

	const guardedCount = 6
	const trustedCount = 6
	var wg sync.WaitGroup
	guardedErrs := make(chan error, guardedCount)
	trustedErrs := make(chan error, trustedCount)
	// Audit отмечает завершение первой записи, не критическую фазу commit.
	// Точное перекрытие разных stores проверяет settings_authority_overlap_test.go.
	wg.Add(1)
	go func() {
		defer wg.Done()
		trustedErrs <- s.SetConfigFieldWithAuthority(ScopeGlobal, "ui_key_a", 0, SettingsAuthorityTrustedUI)
	}()
	awaitAudit(t, audits)
	auditMu.Lock()
	require.NotEmpty(t, auditEvents)
	newest := auditEvents[len(auditEvents)-1]
	auditMu.Unlock()
	require.Equal(t, "ui", newest.Source, "the barrier event must be the trusted write's audit")
	require.NotEmpty(t, newest.Keys)
	require.Equal(t, "ui_key_a", newest.Keys[0])

	for i := 0; i < guardedCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			guardedErrs <- s.SetConfigFieldWithAuthority(ScopeGlobal, "guarded_key_"+string(rune('a'+i)), i, SettingsAuthorityGuarded)
		}(i)
	}
	for i := 1; i < trustedCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			trustedErrs <- s.SetConfigFieldWithAuthority(ScopeGlobal, "ui_key_"+string(rune('a'+i)), i, SettingsAuthorityTrustedUI)
		}(i)
	}
	wg.Wait()
	drainAudits := func() {
		for {
			select {
			case <-audits:
			default:
				return
			}
		}
	}
	drainAudits()
	auditMu.Lock()
	snapshot := append([]SettingsEvent(nil), auditEvents...)
	auditMu.Unlock()
	uiKeys := map[string]int{}
	guardedKeys := map[string]int{}
	for _, e := range snapshot {
		if len(e.Keys) == 0 {
			continue
		}
		if e.Source == "ui" {
			uiKeys[e.Keys[0]]++
		} else if e.Source == "" {
			guardedKeys[e.Keys[0]]++
		}
	}
	for i := 0; i < trustedCount; i++ {
		require.Equal(t, 1, uiKeys["ui_key_"+string(rune('a'+i))], "exactly one ui audit event per trusted key")
	}
	for i := 0; i < guardedCount; i++ {
		require.Equal(t, 1, guardedKeys["guarded_key_"+string(rune('a'+i))], "exactly one guarded audit event per guarded key")
	}
	close(guardedErrs)
	close(trustedErrs)
	for err := range guardedErrs {
		require.ErrorIs(t, err, ErrSettingsLocked)
	}
	for err := range trustedErrs {
		require.NoError(t, err)
	}

	afterBytes := fileBytes(t, p)
	requireSyncRevUnchanged(t, lockedBytes, afterBytes)
	for i := 0; i < trustedCount; i++ {
		require.True(t, gjson.GetBytes(afterBytes, "ui_key_"+string(rune('a'+i))).Exists(), "trusted key %d must be on disk", i)
	}
	for i := 0; i < guardedCount; i++ {
		require.False(t, gjson.GetBytes(afterBytes, "guarded_key_"+string(rune('a'+i))).Exists(), "guarded key %d must not be on disk", i)
	}
}

// Defect caught: every MCP persister's trusted-UI variant must apply under
// the global lock (add/disable/replace/remove, disk + in-memory), while the
// guarded persister is refused and leaves both disk and the published
// in-memory MCP view untouched.
func TestTrustedUIMCPPersistersApplyUnderLock(t *testing.T) {
	restore := cheapPasswordIterationsForTest()
	defer restore()
	s, _ := isolatedMCPConfigStore(t)
	t.Cleanup(func() { SetProcessPassword("") })
	p := GlobalConfigData()
	require.NoError(t, s.LockSettings(ScopeGlobal, trustedUIFixturePassword))
	lockedBytes := fileBytes(t, p)
	syncRev := gjsonString(lockedBytes, "sync_rev")
	serverBefore := MCPConfig{Type: MCPHttp, URL: "http://x"}

	// Guarded persistence refused: disk AND in-memory MCP view unchanged.
	err := s.PersistMCPConfig(ScopeGlobal, "srv", serverBefore)
	require.ErrorIs(t, err, ErrSettingsLocked)
	require.Equal(t, lockedBytes, fileBytes(t, p))
	require.Empty(t, s.Config().MCP, "a refused persistence must not mutate the in-memory MCP view")

	// Trusted add lands on disk and in memory.
	_, err = s.PersistMCPConfigResultWithAuthority(ScopeGlobal, "srv", serverBefore, SettingsAuthorityTrustedUI)
	require.NoError(t, err)
	afterAdd := fileBytes(t, p)
	require.Equal(t, "http://x", gjsonString(afterAdd, "mcp.srv.url"))
	require.Equal(t, syncRev, gjsonString(afterAdd, "sync_rev"))
	cfg, ok := s.Config().MCP["srv"]
	require.True(t, ok)
	require.Equal(t, "http://x", cfg.URL)

	// Trusted disable lands, and reports the server exists.
	result, err := s.PersistMCPDisabledOverrideResultWithAuthority(ScopeGlobal, "srv", true, SettingsAuthorityTrustedUI)
	require.NoError(t, err)
	require.True(t, result.NewExists)
	afterDisable := fileBytes(t, p)
	require.True(t, gjson.Get(string(afterDisable), "mcp.srv.disabled").Bool())
	require.Equal(t, syncRev, gjsonString(afterDisable, "sync_rev"))
	require.True(t, s.Config().MCP["srv"].Disabled)

	// Trusted rename lands: new name on disk and in memory, old one gone.
	result, err = s.PersistReplaceMCPResultWithAuthority(ScopeGlobal, "srv", "srv2", serverBefore, SettingsAuthorityTrustedUI)
	require.NoError(t, err)
	require.Equal(t, "srv2", result.NewName)
	afterReplace := fileBytes(t, p)
	require.Equal(t, "http://x", gjsonString(afterReplace, "mcp.srv2.url"))
	require.False(t, gjson.GetBytes(afterReplace, "mcp.srv").Exists())
	require.Equal(t, syncRev, gjsonString(afterReplace, "sync_rev"))
	_, hasOld := s.Config().MCP["srv"]
	_, hasNew := s.Config().MCP["srv2"]
	require.False(t, hasOld)
	require.True(t, hasNew)

	// Trusted remove lands: key gone from disk and memory, sync_rev intact.
	result, err = s.PersistRemoveMCPConfigResultWithAuthority(ScopeGlobal, "srv2", SettingsAuthorityTrustedUI)
	require.NoError(t, err)
	require.False(t, result.NewExists)
	afterRemove := fileBytes(t, p)
	require.False(t, gjson.GetBytes(afterRemove, "mcp.srv2").Exists())
	require.Equal(t, syncRev, gjsonString(afterRemove, "sync_rev"))
	_, hasAny := s.Config().MCP["srv2"]
	require.False(t, hasAny)
}

// Defect caught: the audit Source label must distinguish transports exactly
// — "ui" for trusted writes (including reserved-key refusals), "" for every
// guarded write — and must never leak the lock password.
func TestTrustedUIAuditSourceLabel(t *testing.T) {
	restore := cheapPasswordIterationsForTest()
	defer restore()
	pinKtavAutoMigrateOffForLegacyTest(t)
	p := filepath.Join(t.TempDir(), "global.json")
	s := lockStore(t, p)
	t.Cleanup(func() { SetSettingsAuditSink(nil); SetProcessPassword("") })
	var mu sync.Mutex
	var events []SettingsEvent
	checked := make(chan struct{}, 16)
	SetSettingsAuditSink(func(e SettingsEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
		checked <- struct{}{}
	})

	require.NoError(t, s.LockSettings(ScopeGlobal, trustedUIFixturePassword))
	awaitAudit(t, checked)
	require.ErrorIs(t, s.SetConfigField(ScopeGlobal, "guarded_key", "v"), ErrSettingsLocked)
	awaitAudit(t, checked)
	require.NoError(t, s.SetConfigFieldWithAuthority(ScopeGlobal, "trusted_key", "v", SettingsAuthorityTrustedUI))
	awaitAudit(t, checked)
	require.NoError(t, s.UnlockSettings(ScopeGlobal, trustedUIFixturePassword))
	awaitAudit(t, checked)
	require.NoError(t, s.SetConfigField(ScopeGlobal, "plain_key", "v"))
	awaitAudit(t, checked)
	require.Error(t, s.SetConfigFieldWithAuthority(ScopeGlobal, "sync_rev", map[string]any{}, SettingsAuthorityTrustedUI))
	awaitAudit(t, checked)

	mu.Lock()
	got := append([]SettingsEvent(nil), events...)
	mu.Unlock()
	lastSource := func(key string) string {
		for i := len(got) - 1; i >= 0; i-- {
			e := got[i]
			if len(e.Keys) > 0 && e.Keys[0] == key {
				return e.Source
			}
		}
		t.Fatalf("no audit event for %q (got %d events)", key, len(got))
		return ""
	}
	require.Equal(t, "", lastSource("guarded_key"), "a guarded refused write must audit with an empty Source")
	require.Equal(t, "ui", lastSource("trusted_key"), "a trusted applied write must audit with Source ui")
	require.Equal(t, "", lastSource("plain_key"), "a guarded applied write must audit with an empty Source")
	// The "*" lookup targets the LAST "*" event: the trusted reserved-key
	// refusal. The lock/unlock events also emit "*" earlier, but with an
	// empty Source.
	require.Equal(t, "ui", lastSource("*"), "a trusted reserved-key refusal must still audit with Source ui")
	starSources := make([]string, 0, len(got))
	for _, e := range got {
		if len(e.Keys) > 0 && e.Keys[0] == "*" {
			starSources = append(starSources, e.Source)
		}
	}
	require.Equal(t, []string{"", "", "ui"}, starSources, "the lock, unlock, and trusted-reserved-refusal events must audit in order with Sources \"\", \"\", and \"ui\"")

	for _, e := range got {
		require.Contains(t, []string{"", "ui"}, e.Source, "Source must be exactly the fixed ui token or empty")
		require.False(t, strings.Contains(e.Source, trustedUIFixturePassword), "Source must never contain the lock password")
	}
}
