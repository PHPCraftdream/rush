package config

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/PHPCraftdream/rush/internal/oauth/codex"
	"github.com/stretchr/testify/require"
)

// Revert-check: refreshCodexOAuthToken (:63) must serialize same-store token exchange.
func TestOAuthRefreshRace_SameStoreSingleUseToken(t *testing.T) {
	runOAuthRefreshSingleUseRace(t, 8, false)
}

// Revert-check: refreshCodexOAuthLeader (:103) must lock shared-config token exchange.
func TestOAuthRefreshRace_TwoStoresSingleUseToken(t *testing.T) {
	runOAuthRefreshSingleUseRace(t, 2, true)
}

type oauthRefreshLoopbackTransport struct {
	endpoint *url.URL
	base     http.RoundTripper
}

func (rt oauthRefreshLoopbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Redirect only the Codex endpoint to the local test server.
	if req.URL.Scheme != "https" || req.URL.Host != "auth.openai.com" || req.URL.Path != "/oauth/token" || req.Method != http.MethodPost {
		return nil, fmt.Errorf("unexpected OAuth test request destination")
	}
	local := req.Clone(req.Context())
	localURL := *rt.endpoint
	local.URL = &localURL
	local.Host = rt.endpoint.Host
	return rt.base.RoundTrip(local)
}

func runOAuthRefreshSingleUseRace(t *testing.T, callers int, separateStores bool) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "rush.json")
	oldToken := &oauth.Token{
		AccessToken:  "single-use-old-access",
		RefreshToken: "single-use-old-refresh",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
		AccountID:    "test-account",
	}
	content, err := json.Marshal(map[string]any{
		"providers": map[string]any{
			"openai-codex": map[string]any{"oauth": oldToken},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, content, 0o600))

	makeStore := func() *ConfigStore {
		providers := csync.NewMap[string, ProviderConfig]()
		providers.Set("openai-codex", ProviderConfig{ID: "openai-codex", OAuthToken: oldToken})
		return newTestConfigStore(testStoreOpts{
			config:         &Config{Providers: providers},
			globalDataPath: configPath,
		})
	}
	stores := make([]*ConfigStore, callers)
	stores[0] = makeStore()
	for i := 1; i < callers; i++ {
		stores[i] = stores[0]
		if separateStores {
			stores[i] = makeStore()
		}
	}

	firstArrived := make(chan struct{})
	reuseObserved := make(chan struct{})
	releaseSuccess := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSuccess) }) }
	var reuseOnce sync.Once
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != oldToken.RefreshToken {
			t.Errorf("unexpected OAuth refresh form")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if exchanges.Add(1) != 1 {
			reuseOnce.Do(func() { close(reuseObserved) })
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		close(firstArrived)
		select {
		case <-releaseSuccess:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"single-use-new-access","refresh_token":"single-use-new-refresh","expires_in":3600}`))
	}))
	// LIFO cleanup releases a blocked handler before closing its server.
	t.Cleanup(server.Close)
	t.Cleanup(release)
	endpoint, err := url.Parse(server.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: oauthRefreshLoopbackTransport{
		endpoint: endpoint,
		base:     server.Client().Transport,
	}}

	// Real exchange decoding; global seams require serial tests.
	originalRefresh := codexRefreshTokenFn
	codexRefreshTokenFn = codex.Refresh
	t.Cleanup(func() { codexRefreshTokenFn = originalRefresh })
	serialized := make(chan struct{}, callers)
	oldWaiterHook := codexRefreshWaiterHook
	codexRefreshWaiterHook = func() {
		select {
		case serialized <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { codexRefreshWaiterHook = oldWaiterHook })
	var lockAttempts atomic.Int32
	hookCodexRefreshLock(t, func(ctx context.Context, path string) (*filelock.FileLock, error) {
		if strings.Contains(path, ".oauth-refresh-") && lockAttempts.Add(1) > 1 {
			select {
			case serialized <- struct{}{}:
			default:
			}
		}
		return filelock.AcquireFileLockContext(ctx, path)
	})
	for _, store := range stores {
		trackOAuthJournalTestStore(t, store)
	}
	idle := hookOAuthJournalIdle(t, stores[0])
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	results := make(chan error, callers)
	var workers sync.WaitGroup
	t.Cleanup(workers.Wait)
	// Cancel and unblock workers before waiting or restoring the global seam.
	t.Cleanup(func() {
		cancel()
		release()
	})
	launch := func(index int, entered chan<- struct{}) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if entered != nil {
				entered <- struct{}{}
			}
			results <- stores[index].RefreshOAuthTokenWithClient(ctx, ScopeGlobal, ProviderConfig{
				ID:         "openai-codex",
				OAuthToken: oldToken,
			}, client)
		}()
	}
	launch(0, nil)
	select {
	case <-firstArrived:
	case <-ctx.Done():
		t.Fatal("first refresh never reached the loopback endpoint")
	}

	// Hold the consumed token until reuse or a serialized follower is observed.
	entered := make(chan struct{}, callers-1)
	for i := 1; i < callers; i++ {
		launch(i, entered)
	}
	for i := 1; i < callers; i++ {
		<-entered
	}
	select {
	case <-reuseObserved:
	case <-serialized:
	case <-ctx.Done():
		t.Fatal("refresh race exceeded its safety budget")
	}
	release()

	for i := 0; i < callers; i++ {
		select {
		case err := <-results:
			assertOAuthRefreshRaceNoError(t, err)
		case <-ctx.Done():
			t.Fatal("refresh callers did not finish")
		}
	}
	workers.Wait()
	waitOAuthJournalIdle(t, idle)
	require.Equal(t, int32(1), exchanges.Load(), "a single-use token must be exchanged exactly once")
	for _, store := range stores {
		current, ok := store.loadSnapshot().config.Providers.Get("openai-codex")
		require.True(t, ok)
		require.NotNil(t, current.OAuthToken)
		require.Equal(t, "single-use-new-access", current.OAuthToken.AccessToken)
		require.Equal(t, "single-use-new-refresh", current.OAuthToken.RefreshToken)
		require.Equal(t, oldToken.AccountID, current.OAuthToken.AccountID)
		require.False(t, current.OAuthToken.IsExpired())
		require.Empty(t, current.APIKey)
	}
	diskToken, err := stores[0].loadTokenFromDisk(ScopeGlobal, "openai-codex")
	require.NoError(t, err)
	require.NotNil(t, diskToken)
	require.Equal(t, "single-use-new-access", diskToken.AccessToken)
	require.Equal(t, "single-use-new-refresh", diskToken.RefreshToken)
}

func assertOAuthRefreshRaceNoError(t *testing.T, err error) {
	t.Helper()
	// Nonfatal so baseline failures still reach the exchange-count oracle.
	if err != nil {
		t.Errorf("refresh caller failed: %v", err)
	}
}
