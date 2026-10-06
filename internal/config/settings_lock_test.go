package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const secretHash = "24a5cfa44bee3072cadb1fea7af9a0ed638d118d63e21d8f5d6979e314b5318e"

func lockStore(t *testing.T, path string) *ConfigStore {
	t.Helper()
	t.Cleanup(func() { SetProcessPassword("") })
	return newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: path})
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

func TestHashPasswordVectors(t *testing.T) {
	got := HashPassword("secret")
	require.Equal(t, secretHash, got)
	recompute := func(s string) string {
		d := []byte(s)
		for i := 0; i < 10; i++ {
			h := sha256.New()
			_, _ = h.Write(d)
			d = h.Sum(nil)
		}
		return hex.EncodeToString(d)
	}
	require.Equal(t, recompute("secret"), got)
	require.Equal(t, "f65afaf57ea8a40281d0e5a0eab8c510edf01b054c672c416649eeb2e9fb6e1e", HashPassword("pässwörd"))
	require.Equal(t, recompute(""), HashPassword(""))
}

// revert-check RC8: injecting a key into Reason must fail the explicit audit-field checks.
func TestSettingsLock_SlogRedactsEventDetails(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	p := filepath.Join(t.TempDir(), "global.json")
	s := lockStore(t, p)
	t.Cleanup(func() { SetSettingsAuditSink(nil) })
	var captured SettingsEvent
	SetSettingsAuditSink(func(event SettingsEvent) {
		captured = event
		logger.Info("settings event", "event", event)
	})
	require.NoError(t, s.LockSettings(ScopeGlobal, "secret"))
	require.NoError(t, s.UnlockSettings(ScopeGlobal, "secret"))
	require.NoError(t, s.LockSettings(ScopeGlobal, "secret"))
	err := s.SetConfigField(ScopeGlobal, "models.private", SelectedModel{Provider: "private-provider", Model: "private-model"})
	require.ErrorIs(t, err, ErrSettingsLocked)
	require.EqualError(t, err, ErrSettingsLocked.Error())
	text := output.String()
	for _, secret := range []string{secretHash, "sync_rev", "sha256", "hash", "local lock", "global lock", "settings are locked", "private-provider", "private-model", "models.private"} {
		require.NotContains(t, text, secret)
	}
	require.Contains(t, text, "global")
	require.Contains(t, text, "blocked")
	for _, sensitive := range []string{"sync_rev", "sha256", "hash", secretHash, "lock"} {
		require.NotContains(t, strings.ToLower(strings.Join(captured.Keys, " ")), strings.ToLower(sensitive))
	}
	require.NotContains(t, strings.ToLower(captured.Reason), "lock")
	for _, sensitive := range []string{"sync_rev", "sha256", "hash", secretHash} {
		require.NotContains(t, strings.ToLower(captured.Reason), strings.ToLower(sensitive))
	}
}

func TestSettingsLock_BestEffortRemovalHonorsLockWithoutAudit(t *testing.T) {
	_, _ = isolateAllGlobalConfigPaths(t)
	p := filepath.Join(t.TempDir(), "global.json")
	s := lockStore(t, p)
	require.NoError(t, s.SetConfigField(ScopeGlobal, "legacy", "keep"))
	require.NoError(t, s.LockSettings(ScopeGlobal, "secret"))
	before := fileBytes(t, p)
	audits := 0
	SetSettingsAuditSink(func(SettingsEvent) { audits++ })
	t.Cleanup(func() { SetSettingsAuditSink(nil) })
	s.removeConfigFieldBestEffort(ScopeGlobal, "legacy")
	require.Equal(t, before, fileBytes(t, p))
	require.Zero(t, audits)
}

func TestSettingsLock_ReservedRemovalErrorsAndRedactsEventKeys(t *testing.T) {
	_, _ = isolateAllGlobalConfigPaths(t)
	p := filepath.Join(t.TempDir(), "global.json")
	s := lockStore(t, p)
	require.NoError(t, s.SetConfigField(ScopeGlobal, "seed", 1))
	var event SettingsEvent
	SetSettingsAuditSink(func(e SettingsEvent) { event = e })
	t.Cleanup(func() { SetSettingsAuditSink(nil) })
	err := s.RemoveConfigField(ScopeGlobal, "sync_rev.secret")
	require.ErrorIs(t, err, ErrSettingsLocked)
	require.EqualError(t, err, ErrSettingsLocked.Error())
	require.Equal(t, []string{"*"}, event.Keys)
	require.NotContains(t, strings.Join(event.Keys, " "), "sync_rev")

	missing := filepath.Join(t.TempDir(), "missing.json")
	empty := lockStore(t, missing)
	require.ErrorIs(t, empty.RemoveConfigField(ScopeGlobal, "sync_rev"), ErrSettingsLocked)
	require.NoFileExists(t, missing)
}

func TestSettingsLock_RecentModelRemovalHasNoAudit(t *testing.T) {
	_, _ = isolateAllGlobalConfigPaths(t)
	p := filepath.Join(t.TempDir(), "global.json")
	s := lockStore(t, p)
	require.NoError(t, s.SetConfigField(ScopeGlobal, "recent_models.smart", []SelectedModel{{Provider: "x", Model: "y"}}))
	audits := 0
	SetSettingsAuditSink(func(SettingsEvent) { audits++ })
	t.Cleanup(func() { SetSettingsAuditSink(nil) })
	require.NoError(t, s.RemoveConfigField(ScopeGlobal, "recent_models.smart"))
	require.Zero(t, audits)
}

func TestSettingsLock_BlocksSetConfigFieldWhileLocked(t *testing.T) {
	p := filepath.Join(t.TempDir(), "global.json")
	s := lockStore(t, p)
	require.NoError(t, s.SetConfigField(ScopeGlobal, "foo", "bar"))
	require.NoError(t, s.LockSettings(ScopeGlobal, "secret"))
	locked, e := s.SettingsLocked()
	require.NoError(t, e)
	require.True(t, locked)
	b := fileBytes(t, p)
	require.Equal(t, secretHash, gjsonString(b, "sync_rev"))
	err := s.SetConfigField(ScopeGlobal, "models.smart", SelectedModel{Provider: "x", Model: "y"})
	require.ErrorIs(t, err, ErrSettingsLocked)
	require.Equal(t, b, fileBytes(t, p))
}

func TestSettingsLock_BlocksRemoveConfigFieldWhileLocked(t *testing.T) {
	p := filepath.Join(t.TempDir(), "g.json")
	s := lockStore(t, p)
	require.NoError(t, s.SetConfigField(ScopeGlobal, "models.smart", SelectedModel{Provider: "x", Model: "y"}))
	require.NoError(t, s.LockSettings(ScopeGlobal, "secret"))
	b := fileBytes(t, p)
	require.ErrorIs(t, s.RemoveConfigField(ScopeGlobal, "models.smart"), ErrSettingsLocked)
	require.Equal(t, b, fileBytes(t, p))
}

func TestSettingsLock_BlocksUpdatePreferredModelsWhileLocked(t *testing.T) {
	p := filepath.Join(t.TempDir(), "g.json")
	s := lockStore(t, p)
	require.NoError(t, s.LockSettings(ScopeGlobal, "secret"))
	b := fileBytes(t, p)
	require.ErrorIs(t, s.UpdatePreferredModels(ScopeGlobal, map[SelectedModelType]SelectedModel{SelectedModelTypeFast: {Provider: "x", Model: "y"}}), ErrSettingsLocked)
	require.Equal(t, b, fileBytes(t, p))
}

func TestSettingsLock_AuthorizedProcessWrites(t *testing.T) {
	_, _ = isolateAllGlobalConfigPaths(t)
	p := GlobalConfigData()
	s := lockStore(t, p)
	require.NoError(t, CheckPasswordAgainstDisk("anything"))
	require.NoError(t, s.LockSettings(ScopeGlobal, "secret"))
	require.NoError(t, CheckPasswordAgainstDisk("secret"))
	require.ErrorIs(t, CheckPasswordAgainstDisk("wrong"), ErrWrongPassword)
	SetProcessPassword("secret")
	require.NoError(t, s.SetConfigField(ScopeGlobal, "models.smart", SelectedModel{Provider: "x", Model: "y"}))
	SetProcessPassword("nope")
	require.ErrorIs(t, s.SetConfigField(ScopeGlobal, "models.fast", "x"), ErrSettingsLocked)
}

// revert-check RC4: removing the recent-model exemption must fail this test.
func TestSettingsLock_RecentModelsExemptWhileLocked(t *testing.T) {
	p := filepath.Join(t.TempDir(), "g.json")
	s := lockStore(t, p)
	require.NoError(t, s.LockSettings(ScopeGlobal, "secret"))
	m := SelectedModel{Provider: "zai", Model: "m"}
	require.NoError(t, s.RecordRecentModel(ScopeGlobal, SelectedModelTypeSmart, m))
	require.Contains(t, string(fileBytes(t, p)), "recent_models")
	b := fileBytes(t, p)
	err := s.SetConfigFields(ScopeGlobal, map[string]any{"recent_models.smart": []SelectedModel{m}, "models.fast": m})
	require.ErrorIs(t, err, ErrSettingsLocked)
	require.Equal(t, b, fileBytes(t, p))
}

func TestSettingsLock_OAuthRefreshExemptWhileLocked(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	path := GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"providers":{"hyper":{"api_key":"newer-access-token","oauth":{"access_token":"newer-access-token","refresh_token":"refresh-abc","expires_in":3600,"expires_at":9999999999}}}}`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("hyper", ProviderConfig{ID: "hyper", APIKey: "older-access-token", OAuthToken: &oauth.Token{AccessToken: "older-access-token", RefreshToken: "refresh-abc", ExpiresIn: 3600, ExpiresAt: time.Now().Add(-time.Hour).Unix()}})
	s := newTestConfigStore(testStoreOpts{config: &Config{Providers: providers}, globalDataPath: path})
	require.NoError(t, s.LockSettings(ScopeGlobal, "pw"))
	require.NoError(t, s.RefreshOAuthToken(context.Background(), ScopeGlobal, "hyper"))
	got, ok := s.Config().Providers.Get("hyper")
	require.True(t, ok)
	require.Equal(t, "newer-access-token", got.APIKey)
	require.ErrorIs(t, s.SetConfigField(ScopeGlobal, "providers.hyper.oauth", map[string]any{"access_token": "bad"}), ErrSettingsLocked)
}

func TestSettingsLock_SettingsLockKeyProtection(t *testing.T) {
	p := filepath.Join(t.TempDir(), "g.json")
	s := lockStore(t, p)
	require.NoError(t, s.SetConfigField(ScopeGlobal, "seed", 1))
	for _, f := range []func() error{func() error { return s.SetConfigField(ScopeGlobal, "sync_rev", map[string]any{}) }, func() error { return s.RemoveConfigField(ScopeGlobal, "sync_rev") }} {
		require.ErrorIs(t, f(), errSettingsLockKeyReserved)
	}
	require.NoError(t, s.LockSettings(ScopeGlobal, "secret"))
	SetProcessPassword("secret")
	for _, f := range []func() error{func() error { return s.SetConfigField(ScopeGlobal, "sync_rev", map[string]any{}) }, func() error { return s.RemoveConfigField(ScopeGlobal, "sync_rev") }} {
		require.ErrorIs(t, f(), errSettingsLockKeyReserved)
	}
	require.NoError(t, s.LockSettings(ScopeGlobal, "new"))
	require.Equal(t, HashPassword("new"), gjsonString(fileBytes(t, p), "sync_rev"))
	empty := newTestConfigStore(testStoreOpts{globalDataPath: filepath.Join(t.TempDir(), "x")})
	require.ErrorIs(t, empty.LockSettings(ScopeWorkspace, "x"), ErrNoWorkspaceConfig)
}

// revert-check RC3: removing the MCP transaction guard must fail this test.
func TestSettingsLock_MCPPersistRefusedWhileLocked(t *testing.T) {
	s, _ := isolatedMCPConfigStore(t)
	p := GlobalConfigData()
	require.NoError(t, s.PersistMCPConfig(ScopeGlobal, "srv", MCPConfig{Type: MCPHttp, URL: "http://x"}))
	require.NoError(t, s.LockSettings(ScopeGlobal, "pw"))
	b := fileBytes(t, p)
	require.ErrorIs(t, s.PersistMCPConfig(ScopeGlobal, "srv2", MCPConfig{Type: MCPHttp, URL: "http://x"}), ErrSettingsLocked)
	require.Equal(t, b, fileBytes(t, p))
	SetProcessPassword("pw")
	t.Cleanup(func() { SetProcessPassword("") })
	require.NoError(t, s.PersistMCPConfig(ScopeGlobal, "srv2", MCPConfig{Type: MCPHttp, URL: "http://x"}))
}

func TestSettingsLock_InvisibleInConfigJSON(t *testing.T) {
	s := lockStore(t, filepath.Join(t.TempDir(), "g.json"))
	require.NoError(t, s.LockSettings(ScopeGlobal, "secret"))
	b, e := json.Marshal(s.Config())
	require.NoError(t, e)
	require.NotContains(t, string(b), "sync_rev")
	require.NotContains(t, string(b), secretHash)
}

// revert-check RC5: restoring the Load self-heal disk write must fail this test.
func TestLoad_SelfHealDoesNotTouchDisk(t *testing.T) {
	dir := t.TempDir()
	_, globalDataDir := isolateAllGlobalConfigPaths(t)
	resetProviderState()
	t.Cleanup(resetProviderState)
	path := filepath.Join(dir, "rush.json")
	seed := `{"options":{"disable_default_providers":true},"models":{"smart":{"provider":"ghost-provider","model":"not-a-real-model"}},"providers":{"openai":{"api_key":"test","base_url":"https://example.invalid/v1","models":[{"id":"gpt-4","name":"GPT-4"}]}}}`
	require.NoError(t, os.WriteFile(path, []byte(seed), 0o600))
	global := filepath.Join(globalDataDir, "rush.json")
	require.ErrorIs(t, func() error { _, e := os.Stat(global); return e }(), os.ErrNotExist)
	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	smart := store.Config().Models[SelectedModelTypeSmart]
	require.Equal(t, SelectedModel{Provider: "openai", Model: "gpt-4"}, smart)
	_, err = os.Stat(global)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// revert-check RC6: removing synchronization between lock checks and writes must fail this test.
func TestSettingsLock_ConcurrentLockVersusWriters(t *testing.T) {
	s := lockStore(t, filepath.Join(t.TempDir(), "g.json"))
	var stop atomic.Bool
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				e := s.SetConfigField(ScopeGlobal, "models.fast", SelectedModel{Provider: "x", Model: "y"})
				if e != nil && !errors.Is(e, ErrSettingsLocked) {
					errs <- e
					return
				}
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, s.LockSettings(ScopeGlobal, "pw"))
	stop.Store(true)
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	for i := 0; i < 10; i++ {
		require.ErrorIs(t, s.SetConfigField(ScopeGlobal, "models.fast", "x"), ErrSettingsLocked)
	}
}

func TestSettingsLock_GlobalLockBlocksAllWorkspaces(t *testing.T) {
	root := t.TempDir()
	gp := filepath.Join(root, "global.json")
	a := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: gp, workspacePath: filepath.Join(root, "a", "rush.json")})
	b := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: gp, workspacePath: filepath.Join(root, "b", "rush.json")})
	require.NoError(t, a.LockSettings(ScopeGlobal, "gpass"))
	for _, s := range []*ConfigStore{a, b} {
		for _, scope := range []Scope{ScopeGlobal, ScopeWorkspace} {
			require.ErrorIs(t, s.SetConfigField(scope, "foo", "x"), ErrSettingsLocked)
		}
	}
}

func TestSettingsLock_LocalLockOnlyBlocksItsWorkspace(t *testing.T) {
	root := t.TempDir()
	gp := filepath.Join(root, "g.json")
	a := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: gp, workspacePath: filepath.Join(root, "a.json")})
	b := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: gp, workspacePath: filepath.Join(root, "b.json")})
	require.NoError(t, a.LockSettings(ScopeWorkspace, "lpass"))
	require.ErrorIs(t, a.SetConfigField(ScopeGlobal, "foo", "x"), ErrSettingsLocked)
	require.ErrorIs(t, a.SetConfigField(ScopeWorkspace, "foo", "x"), ErrSettingsLocked)
	require.NoError(t, b.SetConfigField(ScopeGlobal, "foo", "x"))
	locked, e := b.SettingsLocked()
	require.NoError(t, e)
	require.False(t, locked)
}

func TestSettingsLock_GlobalLockOverridesLocalPassword(t *testing.T) {
	root := t.TempDir()
	gp := filepath.Join(root, "g.json")
	a := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: gp, workspacePath: filepath.Join(root, "a.json")})
	t.Cleanup(func() { SetProcessPassword("") })
	require.NoError(t, a.LockSettings(ScopeGlobal, "gpass"))
	require.ErrorIs(t, a.LockSettings(ScopeWorkspace, "lpass"), ErrSettingsLocked)
	SetProcessPassword("gpass")
	require.NoError(t, a.LockSettings(ScopeWorkspace, "lpass"))
	SetProcessPassword("")
	require.ErrorIs(t, a.SetConfigField(ScopeGlobal, "foo", "x"), ErrSettingsLocked)
	SetProcessPassword("lpass")
	require.ErrorIs(t, a.SetConfigField(ScopeGlobal, "foo", "x"), ErrSettingsLocked)
	require.ErrorIs(t, a.CheckProcessPassword(), ErrWrongPassword)
	SetProcessPassword("gpass")
	require.NoError(t, a.SetConfigField(ScopeGlobal, "foo", "x"))
	require.NoError(t, a.CheckProcessPassword())
	SetProcessPassword("nope")
	require.ErrorIs(t, a.CheckProcessPassword(), ErrWrongPassword)
}

func TestSettingsLock_LocalUnlockUnderGlobalLock(t *testing.T) {
	root := t.TempDir()
	gp := filepath.Join(root, "g.json")
	ws := filepath.Join(root, "a.json")
	a := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: gp, workspacePath: ws})
	t.Cleanup(func() { SetProcessPassword("") })
	require.NoError(t, a.LockSettings(ScopeGlobal, "gpass"))
	SetProcessPassword("gpass")
	require.NoError(t, a.LockSettings(ScopeWorkspace, "lpass"))
	SetProcessPassword("")
	require.ErrorIs(t, a.UnlockSettings(ScopeWorkspace, "lpass"), ErrSettingsLocked)
	state, e := a.LockState()
	require.NoError(t, e)
	require.True(t, state.Local)
	SetProcessPassword("gpass")
	require.NoError(t, a.UnlockSettings(ScopeWorkspace, "lpass"))
	state, e = a.LockState()
	require.NoError(t, e)
	require.False(t, state.Local)
	require.ErrorIs(t, a.UnlockSettings(ScopeGlobal, "lpass"), ErrWrongPassword)
	require.NoError(t, a.UnlockSettings(ScopeGlobal, "gpass"))
}

func TestSettingsLock_ForeignWorkspaceKeyIgnored(t *testing.T) {
	root := t.TempDir()
	gp := filepath.Join(root, "g.json")
	wa := filepath.Join(root, "a.json")
	wb := filepath.Join(root, "b.json")
	a := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: gp, workspacePath: wa})
	b := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: gp, workspacePath: wb})
	naked := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: gp})
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(wb, []byte(`{"sync_rev":"`+HashPassword("other")+`"}`), 0o600))
	require.NoError(t, a.SetConfigField(ScopeGlobal, "foo", "a"))
	require.NoError(t, a.SetConfigField(ScopeWorkspace, "foo", "a"))
	require.NoError(t, naked.SetConfigField(ScopeGlobal, "foo", "n"))
	require.ErrorIs(t, b.SetConfigField(ScopeWorkspace, "foo", "b"), ErrSettingsLocked)
}

func gjsonString(b []byte, path string) string { return gjson.GetBytes(b, path).String() }
