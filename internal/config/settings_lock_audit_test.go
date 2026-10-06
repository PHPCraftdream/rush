package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestSettingsLock_RefusalMessagesInstructAgent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "g.json")
	s := lockStore(t, p)
	require.NoError(t, s.SetConfigField(ScopeGlobal, "seed", 1))
	require.NoError(t, s.LockSettings(ScopeGlobal, "secret"))
	errs := []error{s.SetConfigField(ScopeGlobal, "models.smart", SelectedModel{Provider: "x", Model: "y"}), s.RemoveConfigField(ScopeGlobal, "models.smart"), s.UpdatePreferredModels(ScopeGlobal, map[SelectedModelType]SelectedModel{SelectedModelTypeFast: {Provider: "x", Model: "y"}})}
	mcp, _ := isolatedMCPConfigStore(t)
	require.NoError(t, mcp.LockSettings(ScopeGlobal, "pw"))
	errs = append(errs, mcp.PersistMCPConfig(ScopeGlobal, "srv", MCPConfig{Type: MCPHttp, URL: "http://x"}))
	for _, err := range errs {
		require.ErrorIs(t, err, ErrSettingsLocked)
		text := strings.ToLower(err.Error())
		require.Contains(t, text, "forbidden by the user")
		require.Contains(t, text, "ask the user")
		for _, bad := range []string{"password", "--password", "unlock", "sha256", HashPassword("secret")} {
			require.NotContains(t, text, strings.ToLower(bad))
		}
	}
}

// revert-check RC7: restoring lock-specific refusal reasons must fail TestSettingsLock_SlogRedactsEventDetails.
func TestSettingsLock_AuditEvents(t *testing.T) {
	dir := t.TempDir()
	gp := filepath.Join(dir, "global.json")
	ws := filepath.Join(dir, "workspace.json")
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("acme", ProviderConfig{ID: "acme", APIKey: "super-secret-key-123"})
	s := newTestConfigStore(testStoreOpts{config: &Config{Providers: providers}, globalDataPath: gp, workspacePath: ws})
	t.Cleanup(func() { SetSettingsAuditSink(nil); SetProcessPassword("") })
	var mu sync.Mutex
	events := []SettingsEvent{}
	checked := make(chan struct{}, 16)
	var got []SettingsEvent
	SetSettingsAuditSink(func(e SettingsEvent) {
		_, _ = s.SettingsLocked()
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
		checked <- struct{}{}
	})
	require.NoError(t, s.SetConfigField(ScopeGlobal, "models.smart", SelectedModel{Provider: "zai", Model: "glm-5"}))
	awaitAudit(t, checked)
	require.NoError(t, s.LockSettings(ScopeGlobal, "pw"))
	awaitAudit(t, checked)
	require.ErrorIs(t, s.SetConfigField(ScopeGlobal, "models.fast", SelectedModel{Provider: "zai", Model: "glm-6"}), ErrSettingsLocked)
	awaitAudit(t, checked)
	require.NoError(t, s.UnlockSettings(ScopeGlobal, "pw"))
	awaitAudit(t, checked)
	require.NoError(t, s.SetConfigField(ScopeGlobal, "foo", "bar"))
	awaitAudit(t, checked)
	require.NoError(t, s.LockSettings(ScopeWorkspace, "lpw"))
	awaitAudit(t, checked)
	require.ErrorIs(t, s.SetConfigField(ScopeWorkspace, "foo", "bar"), ErrSettingsLocked)
	awaitAudit(t, checked)
	mu.Lock()
	beforeExempt := len(events)
	mu.Unlock()
	SetProcessPassword("pw")
	require.NoError(t, s.RecordRecentModel(ScopeGlobal, SelectedModelTypeSmart, SelectedModel{Provider: "zai", Model: "m"}))
	mu.Lock()
	require.Equal(t, beforeExempt, len(events))
	mu.Unlock()

	oauthPath := filepath.Join(dir, "oauth.json")
	oauthData := `{"providers":{"hyper":{"api_key":"newer-access-token","oauth":{"access_token":"newer-access-token","refresh_token":"refresh-abc","expires_in":3600,"expires_at":9999999999}}}}`
	require.NoError(t, os.WriteFile(oauthPath, []byte(oauthData), 0o600))
	oauthProviders := csync.NewMap[string, ProviderConfig]()
	oauthProviders.Set("hyper", ProviderConfig{ID: "hyper", APIKey: "older-access-token", OAuthToken: &oauth.Token{AccessToken: "older-access-token", RefreshToken: "refresh-abc", ExpiresIn: 3600, ExpiresAt: time.Now().Add(-time.Hour).Unix()}})
	oauthStore := newTestConfigStore(testStoreOpts{config: &Config{Providers: oauthProviders}, globalDataPath: oauthPath})
	SetProcessPassword("pw")
	require.NoError(t, oauthStore.LockSettings(ScopeGlobal, "pw"))
	mu.Lock()
	beforeOAuth := len(events)
	mu.Unlock()
	require.NoError(t, oauthStore.RefreshOAuthToken(context.Background(), ScopeGlobal, "hyper"))
	mu.Lock()
	require.Equal(t, beforeOAuth, len(events))
	mu.Unlock()
	SetProcessPassword("")
	mu.Lock()
	got = append([]SettingsEvent(nil), events...)
	mu.Unlock()
	containsSecret := func(v string) bool {
		return strings.Contains(v, "super-secret-key-123") || strings.Contains(v, "super-secret")
	}
	for _, e := range got {
		require.False(t, containsSecret(e.Scope))
		require.False(t, containsSecret(e.Path))
		require.False(t, containsSecret(e.Outcome))
		require.False(t, containsSecret(e.Reason))
		for _, k := range e.Keys {
			require.False(t, containsSecret(k))
		}
		for k, v := range e.Models {
			require.False(t, containsSecret(k))
			require.False(t, containsSecret(v))
		}
	}
	find := func(outcome, scope, key string) SettingsEvent {
		for _, e := range got {
			if e.Outcome == outcome && e.Scope == scope && len(e.Keys) > 0 && e.Keys[0] == key {
				return e
			}
		}
		t.Fatalf("event not found: %s %s %s", outcome, scope, key)
		return SettingsEvent{}
	}
	applied := find("applied", "global", "models.smart")
	require.Equal(t, map[string]string{"models.smart": "zai/glm-5"}, applied.Models)
	require.Equal(t, "change refused", find("blocked", "global", "models.fast").Reason)
	require.Equal(t, "change refused", find("blocked", "workspace", "foo").Reason)
	require.Equal(t, "applied", find("applied", "global", "*").Outcome)
	require.Equal(t, "applied", find("applied", "workspace", "*").Outcome)
	require.Nil(t, find("applied", "global", "foo").Models)

	if runtime.GOOS == "windows" {
		// The local workspace lock is still present; authenticate using its correct password
		SetProcessPassword("lpw")
		SetSettingsAuditSink(func(e SettingsEvent) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
			checked <- struct{}{}
		})
		configTestHooks.Lock()
		configTestHooks.afterCommitRenamePath = func(path string) error {
			if filepath.Clean(path) == filepath.Clean(gp) {
				return errors.New("injected post-publication error")
			}
			return nil
		}
		configTestHooks.Unlock()
		err := s.SetConfigField(ScopeGlobal, "failure", "x")
		configTestHooks.Lock()
		configTestHooks.afterCommitRenamePath = nil
		configTestHooks.Unlock()
		require.Error(t, err)
		awaitAudit(t, checked)
		mu.Lock()
		got = append([]SettingsEvent(nil), events...)
		mu.Unlock()
		found := false
		for _, e := range got {
			if len(e.Keys) > 0 && e.Keys[0] == "failure" {
				found = e.Outcome == "failed" && e.Reason == "operation failed"
			}
		}
		require.True(t, found)
	} else {
		t.Skip("Windows-only failure injection")
	}
}
func awaitAudit(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("audit sink deadlocked")
	}
}
