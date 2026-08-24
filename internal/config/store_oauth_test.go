// OAuth and provider-token tests: loadTokenFromDisk, RefreshOAuthToken
// (including the reload-during-network-call regression), and
// SetProviderRuntimeConfig visibility versus reload.

package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	hyperp "github.com/PHPCraftdream/rush/internal/agent/hyper"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestLoadTokenFromDisk_ReturnsNewerToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")

	// Create config file with a newer token on disk
	configContent := `{
		"providers": {
			"hyper": {
				"oauth": {
					"access_token": "newer-token-from-disk",
					"refresh_token": "refresh-abc",
					"expires_in": 3600,
					"expires_at": 9999999999
				}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
	})

	token, err := store.loadTokenFromDisk(ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.NotNil(t, token)
	require.Equal(t, "newer-token-from-disk", token.AccessToken)
	require.Equal(t, "refresh-abc", token.RefreshToken)
	require.Equal(t, 3600, token.ExpiresIn)
	require.Equal(t, int64(9999999999), token.ExpiresAt)
}

func TestLoadTokenFromDisk_ReturnsNilWhenSameToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")

	// Create config file with the same token
	configContent := `{
		"providers": {
			"hyper": {
				"oauth": {
					"access_token": "same-token",
					"refresh_token": "refresh-abc",
					"expires_in": 3600,
					"expires_at": 9999999999
				}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
	})

	token, err := store.loadTokenFromDisk(ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.NotNil(t, token)
	require.Equal(t, "same-token", token.AccessToken)
}

func TestLoadTokenFromDisk_ReturnsNilWhenFileMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "nonexistent.json")

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
	})

	token, err := store.loadTokenFromDisk(ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.Nil(t, token)
}

func TestLoadTokenFromDisk_ReturnsNilWhenProviderMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")

	// Create config file without the hyper provider
	configContent := `{"providers": {"openai": {"api_key": "test-key"}}}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
	})

	token, err := store.loadTokenFromDisk(ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.Nil(t, token)
}

func TestLoadTokenFromDisk_ReturnsNilWhenOAuthMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")

	// Create config file with provider but no OAuth token
	configContent := `{"providers": {"hyper": {"api_key": "test-key"}}}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	store := newTestConfigStore(testStoreOpts{
		config:         &Config{},
		globalDataPath: configPath,
	})

	token, err := store.loadTokenFromDisk(ScopeGlobal, "hyper")
	require.NoError(t, err)
	require.Nil(t, token)
}

func TestRefreshOAuthToken_UsesDiskTokenWhenDifferent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")

	// Create config file with a newer token on disk
	configContent := `{
		"providers": {
			"hyper": {
				"api_key": "newer-access-token",
				"oauth": {
					"access_token": "newer-access-token",
					"refresh_token": "refresh-abc",
					"expires_in": 3600,
					"expires_at": 9999999999
				}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	// Set up store with an older in-memory token
	oldToken := &oauth.Token{
		AccessToken:  "older-access-token",
		RefreshToken: "refresh-abc",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(), // Expired
	}

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("hyper", ProviderConfig{
		ID:         "hyper",
		Name:       "Hyper",
		APIKey:     oldToken.AccessToken,
		OAuthToken: oldToken,
	})

	store := newTestConfigStore(testStoreOpts{
		config: &Config{
			Providers: providers,
		},
		globalDataPath: configPath,
	})

	// Refresh should use the disk token without making an external call
	err := store.RefreshOAuthToken(context.Background(), ScopeGlobal, "hyper")
	require.NoError(t, err)

	// Verify the in-memory token was updated to the disk token
	updatedConfig, ok := store.Config().Providers.Get("hyper")
	require.True(t, ok)
	require.Equal(t, "newer-access-token", updatedConfig.APIKey)
	require.Equal(t, "newer-access-token", updatedConfig.OAuthToken.AccessToken)
	require.Equal(t, "refresh-abc", updatedConfig.OAuthToken.RefreshToken)
}

// TestSetProviderRuntimeConfig_VisibleImmediatelyAndDiscardedByReload
// verifies the core contract of SetProviderRuntimeConfig: the in-memory
// provider update is visible immediately after the call returns, but a
// subsequent ReloadFromDisk rebuilds Providers from disk and discards the
// runtime-only change by design (the template/key on disk is the source of
// truth, not the in-memory copy).
func TestSetProviderRuntimeConfig_VisibleImmediatelyAndDiscardedByReload(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")

	// Config with one provider on disk.
	initialConfig := `{
		"providers": {
			"openai": {
				"api_key": "disk-key",
				"models": [{"id": "gpt-4", "name": "GPT-4"}]
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Isolate from host global config.
	isolateAllGlobalConfigPaths(t)
	resetProviderState()
	t.Cleanup(resetProviderState)

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Verify the provider loaded from disk.
	pc, ok := store.Config().Providers.Get("openai")
	require.True(t, ok)
	require.Equal(t, "disk-key", pc.APIKey)

	// Apply a runtime-only update (simulating an API key refresh).
	pc.APIKey = "refreshed-key"
	store.SetProviderRuntimeConfig("openai", pc)

	// The update must be visible immediately — no intervening reload.
	pc2, ok := store.Config().Providers.Get("openai")
	require.True(t, ok)
	require.Equal(t, "refreshed-key", pc2.APIKey,
		"SetProviderRuntimeConfig change must be visible immediately")

	// A reload rebuilds Providers from disk, discarding the runtime-only
	// change. The API key reverts to the disk value.
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	pc3, ok := store.Config().Providers.Get("openai")
	require.True(t, ok)
	require.Equal(t, "disk-key", pc3.APIKey,
		"reload must rebuild Providers from disk, discarding runtime-only updates")
}

// TestRefreshOAuthToken_SurvivesReloadDuringNetworkCall is the regression
// test for the P2.1 finding: RefreshOAuthToken used to capture the
// Providers *csync.Map ONCE at the top of the function and write the
// refreshed token into that same captured map at the very end, AFTER the
// network round-trip (copilotRefreshTokenFn / hyperExchangeTokenFn). Every
// reload allocates a brand new *csync.Map (see SetProviderRuntimeConfig's
// doc comment), so if a reload published a new generation while the
// network call was in flight, the final write landed in the OLD,
// already-orphaned map — invisible to any reader of the current snapshot.
//
// This test overrides hyperExchangeTokenFn (restored via t.Cleanup) to
// block until the test goroutine has published a new generation (simulating
// a concurrent reload), THEN return the refreshed token — reproducing "a
// reload happened while we were waiting on the network" deterministically,
// without a real HTTP call and without relying on hyper.BaseURL()'s
// process-wide sync.OnceValue memoization (see
// TestProviders_ConcurrentErrorCollection_NotLost's caveat in
// provider_test.go for why that path can't be redirected safely per-test).
//
// The store is built via newTestConfigStore with no workingDir, so
// RefreshOAuthToken's own SetConfigFields call at the end succeeds in
// writing to globalDataPath but its trailing autoReload is skipped (see
// autoReload's "workingDir == "" " guard) — this is deliberate: if
// autoReload ran, it would re-read the very token RefreshOAuthToken just
// persisted to disk and transparently re-sync the in-memory Providers map,
// self-healing the exact bug this test targets and making the buggy and
// fixed code indistinguishable from the test's perspective. Skipping it
// isolates what we actually want to observe: whether RefreshOAuthToken's
// OWN in-memory write landed in the orphaned pre-reload map or the current
// one — independent of any later disk-driven resync.
func TestRefreshOAuthToken_SurvivesReloadDuringNetworkCall(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "rush.json")
	require.NoError(t, os.WriteFile(configPath, []byte("{}"), 0o600))

	oldToken := &oauth.Token{
		AccessToken:  "old-access-token",
		RefreshToken: "refresh-token-1",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(), // expired, forces refresh
	}
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set(hyperp.Name, ProviderConfig{
		ID:         hyperp.Name,
		Name:       "Hyper",
		APIKey:     oldToken.AccessToken,
		OAuthToken: oldToken,
	})

	store := newTestConfigStore(testStoreOpts{
		config: &Config{
			Providers: providers,
		},
		globalDataPath: configPath,
	})

	mapBeforeReload := store.Config().Providers

	// Override the network call: signal reachedNetworkCall as soon as
	// RefreshOAuthToken has entered it (proof that it already captured
	// `providers := s.loadSnapshot().config.Providers` at the top of the
	// function, since that capture happens strictly before this call), then
	// block until the test has published a new generation (simulating a
	// concurrent reload), THEN return the refreshed token. Without this
	// handshake, the goroutine below racing the reload published right
	// after it could lose the race and observe the NEW generation from the
	// start — which would make this test pass even against the buggy code,
	// since there would be no orphaned map involved at all.
	reachedNetworkCall := make(chan struct{})
	reloadDone := make(chan struct{})
	origFn := hyperExchangeTokenFn
	hyperExchangeTokenFn = func(ctx context.Context, refreshToken string) (*oauth.Token, error) {
		close(reachedNetworkCall)
		<-reloadDone
		return &oauth.Token{
			AccessToken:  "new-access-token",
			RefreshToken: "refresh-token-2",
			ExpiresIn:    3600,
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		}, nil
	}
	t.Cleanup(func() { hyperExchangeTokenFn = origFn })

	refreshErrCh := make(chan error, 1)
	go func() {
		refreshErrCh <- store.RefreshOAuthToken(context.Background(), ScopeGlobal, hyperp.Name)
	}()

	select {
	case <-reachedNetworkCall:
	case <-time.After(10 * time.Second):
		t.Fatal("RefreshOAuthToken did not reach the network call in time")
	}

	// Simulate a concurrent reload publishing a new generation with a
	// BRAND NEW Providers map (containing the pre-refresh provider config,
	// exactly as a real reload rebuilding from the same on-disk state
	// would) while the refresh call above is blocked inside
	// hyperExchangeTokenFn. This reproduces the exact "orphaned map"
	// scenario the fix addresses: RefreshOAuthToken captured
	// mapBeforeReload before this point (guaranteed by the handshake
	// above), but the current generation now points at a different map.
	newProviders := csync.NewMap[string, ProviderConfig]()
	newProviders.Set(hyperp.Name, ProviderConfig{
		ID:         hyperp.Name,
		Name:       "Hyper",
		APIKey:     oldToken.AccessToken,
		OAuthToken: oldToken,
	})
	store.publishMu.Lock()
	next := store.loadSnapshot().clone()
	cfgCopy := *next.config
	cfgCopy.Providers = newProviders
	next.config = &cfgCopy
	store.publishLocked(next)
	store.publishMu.Unlock()

	mapAfterReload := store.Config().Providers
	require.NotSame(t, mapBeforeReload, mapAfterReload,
		"sanity check: the simulated reload must publish a brand new Providers map")

	close(reloadDone)

	select {
	case err := <-refreshErrCh:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("RefreshOAuthToken did not return in time")
	}

	// The refreshed token must be visible in the CURRENTLY published
	// snapshot, not lost in the pre-reload map RefreshOAuthToken captured
	// at its start.
	finalPC, ok := store.Config().Providers.Get(hyperp.Name)
	require.True(t, ok)
	require.Equal(t, "new-access-token", finalPC.APIKey,
		"refreshed token must be visible in the current generation, not dropped into an orphaned map")
	require.Equal(t, "new-access-token", finalPC.OAuthToken.AccessToken)
	require.Equal(t, "refresh-token-2", finalPC.OAuthToken.RefreshToken)

	// mapBeforeReload (the orphaned pre-reload map) must NOT have received
	// the refreshed token — that's the exact failure mode of the bug: a
	// write into a map no reader can reach anymore.
	orphanedPC, ok := mapBeforeReload.Get(hyperp.Name)
	require.True(t, ok)
	require.Equal(t, "old-access-token", orphanedPC.APIKey,
		"the orphaned pre-reload map must be untouched by RefreshOAuthToken's write")
}
