package config

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestF8OAuthDiskTokenWinsExpiredMemoryUnderGlobalLock(t *testing.T) {
	_, dataDir := isolateAllGlobalConfigPaths(t)
	path := filepath.Join(dataDir, "rush.json")
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	const hash = "preserved-global-hash"
	seed := `{"sync_rev":"` + HashPassword(hash) + `","providers":{"hyper":{"api_key":"disk-access","oauth":{"access_token":"disk-access","refresh_token":"refresh","expires_in":3600,"expires_at":9999999999}}}}`
	require.NoError(t, os.WriteFile(path, []byte(seed), 0o600))
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("hyper", ProviderConfig{ID: "hyper", APIKey: "memory-old", OAuthToken: &oauth.Token{AccessToken: "memory-old", RefreshToken: "refresh", ExpiresIn: 3600, ExpiresAt: time.Now().Add(-time.Hour).Unix()}})
	store := newTestConfigStore(testStoreOpts{config: &Config{Providers: providers}, globalDataPath: path})
	SetProcessPassword("")
	require.NoError(t, store.RefreshOAuthToken(context.Background(), ScopeGlobal, "hyper"))
	got, ok := store.Config().Providers.Get("hyper")
	require.True(t, ok)
	require.Equal(t, "disk-access", got.OAuthToken.AccessToken)
	require.Equal(t, HashPassword(hash), gjsonString(fileBytes(t, path), "sync_rev"))
}

func TestF8OAuthRefreshCASPersistsFreshTokenUnderGlobalLock(t *testing.T) {
	_, dataDir := isolateAllGlobalConfigPaths(t)
	resetProviderState()
	t.Cleanup(resetProviderState)
	path := filepath.Join(dataDir, "rush.json")
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	const hash = "preserved-global-hash"
	seed := `{"sync_rev":"` + HashPassword(hash) + `","providers":{"hyper":{"api_key":"old-access","oauth":{"access_token":"old-access","refresh_token":"refresh","expires_in":3600,"expires_at":1}}}}`
	require.NoError(t, os.WriteFile(path, []byte(seed), 0o600))
	SetProcessPassword("")
	t.Cleanup(func() { SetProcessPassword("") })
	store, err := Load(t.TempDir(), dataDir, false)
	require.NoError(t, err)
	serialized, err := json.Marshal(store.Config())
	require.NoError(t, err)
	require.NotContains(t, string(serialized), "sync_rev")
	require.NotContains(t, string(serialized), HashPassword(hash))
	orig := hyperExchangeTokenFn
	hyperExchangeTokenFn = func(context.Context, *http.Client, string) (*oauth.Token, error) {
		return &oauth.Token{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", ExpiresIn: 7200, ExpiresAt: time.Now().Add(time.Hour).Unix()}, nil
	}
	t.Cleanup(func() { hyperExchangeTokenFn = orig })
	SetProcessPassword("")
	require.NoError(t, store.RefreshOAuthToken(context.Background(), ScopeGlobal, "hyper"))
	var disk struct {
		Providers map[string]struct {
			OAuth struct {
				AccessToken string `json:"access_token"`
			} `json:"oauth"`
		} `json:"providers"`
		SyncRev string `json:"sync_rev"`
	}
	require.NoError(t, json.Unmarshal(fileBytes(t, path), &disk))
	require.Equal(t, "fresh-access", disk.Providers["hyper"].OAuth.AccessToken)
	serialized, err = json.Marshal(store.Config())

	require.NoError(t, err)
	require.NotContains(t, string(serialized), "sync_rev")
	require.NotContains(t, string(serialized), HashPassword(hash))
}

func TestF8GlobalLockRefusesUnauthorizedLockWhenOnlyLocalLockExists(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "global.json")
	workspace := filepath.Join(root, "workspace", ".rush", "rush.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(workspace), 0o700))
	require.NoError(t, os.WriteFile(workspace, []byte(`{"sync_rev":"`+HashPassword("local")+`"}`), 0o600))
	store := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: global, workspacePath: workspace})
	store.workingDir = filepath.Dir(filepath.Dir(filepath.Dir(workspace)))
	SetProcessPassword("")
	t.Cleanup(func() { SetProcessPassword("") })
	before := fileBytes(t, global)
	err := store.LockSettings(ScopeGlobal, "new")
	require.ErrorIs(t, err, ErrSettingsLocked)
	require.Equal(t, before, fileBytes(t, global))
}

func TestF8LocalLockRefusesMCPWorkspaceAndGlobalPersistence(t *testing.T) {
	store, workspace := isolatedMCPConfigStore(t)
	projectPath := filepath.Join(workspace, ".mcp.json")
	writeExternalMCPDefinition(t, projectPath, "existing")
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	lockPath, err := store.configPath(ScopeWorkspace)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(lockPath), 0o700))
	require.NoError(t, os.WriteFile(lockPath, []byte(`{"sync_rev":"`+HashPassword("local")+`"}`), 0o600))
	store.workingDir = workspace
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	SetProcessPassword("")
	t.Cleanup(func() { SetProcessPassword("") })
	// The workspace persist writes the workspace rush.json overlay (the project
	// .mcp.json is only read), so that file is what must stay unchanged.
	before := fileBytes(t, lockPath)
	err = store.PersistMCPDisabledOverrideExact(ScopeWorkspace, "existing", true)
	require.ErrorIs(t, err, ErrSettingsLocked)
	require.Equal(t, before, fileBytes(t, lockPath))
	global := GlobalConfigData()
	globalBefore := fileBytes(t, global)
	err = store.PersistMCPConfig(ScopeGlobal, "new", MCPConfig{Type: MCPHttp, URL: "http://global"})
	require.ErrorIs(t, err, ErrSettingsLocked)
	require.Equal(t, globalBefore, fileBytes(t, global))
}
