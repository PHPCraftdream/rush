package config

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/codexprovider"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

// These tests are serial because the exchange descriptor seam is global.
// Keep the real provider policies; count and reject only unexpected exchanges.
func task1314DiskNoExchange(t *testing.T) *atomic.Int32 {
	t.Helper()
	calls := &atomic.Int32{}
	base := oauthProviderDescriptors()
	old := oauthProviderDescriptorsOverride
	for index := range base {
		base[index].exchange = func(context.Context, *http.Client, string) (*oauth.Token, error) {
			calls.Add(1)
			return nil, errors.New("unexpected task1314 exchange")
		}
	}
	oauthProviderDescriptorsOverride = func() []oauthProviderDescriptor { return base }
	t.Cleanup(func() {
		oauthProviderDescriptorsOverride = old
		require.Zero(t, calls.Load(), "disk polling must never exchange")
	})
	joinOAuthJournalTestStoresBeforeRestore(t)
	return calls
}

type task1314DiskSource struct {
	store *ConfigStore
}

func (s *task1314DiskSource) Current() (oauth.Token, bool) {
	pc, ok := s.store.Config().Providers.Get("openai-codex")
	if !ok || pc.OAuthToken == nil {
		return oauth.Token{}, false
	}
	return *pc.OAuthToken, true
}

func (s *task1314DiskSource) Refresh(ctx context.Context, used oauth.Token) error {
	return s.store.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, ProviderConfig{ID: "openai-codex", OAuthToken: &used}, nil)
}

type task1314DiskRoundTripper func(*http.Request) (*http.Response, error)

func (rt task1314DiskRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return rt(r)
}

func TestTask1314OAuthDiskSyncRealProviderMatrix(t *testing.T) {
	// Revert-check: syncOAuthFromDisk and applyOAuthToken must adopt all three real provider IDs once without exchanging or publishing equal tokens.
	task1314DiskNoExchange(t)
	for _, id := range []string{"openai-codex", "copilot", "hyper"} {
		t.Run(id, func(t *testing.T) {
			f := task1292Fixture(t, id)
			writer := task1292Store(t, f.path, f.used)
			replacement := *f.next
			replacement.AccountID = "fixture-next-account"
			// A second real store persists sibling credentials through the normal writer.
			require.NoError(t, writer.SetConfigFields(ScopeGlobal, map[string]any{"providers." + id + ".oauth": &replacement}))
			require.Equal(t, f.used.OAuthToken, diskSyncLive(t, f.store, id))
			before := f.store.Generation()
			var state oauthDiskSyncState
			f.store.syncOAuthFromDisk(t.Context(), &state)
			require.Equal(t, &replacement, diskSyncLive(t, f.store, id))
			require.Equal(t, before+1, f.store.Generation())
			// A fresh state forces an actual equal-token read, not a stat-gate skip.
			var equalState oauthDiskSyncState
			f.store.syncOAuthFromDisk(t.Context(), &equalState)
			require.True(t, equalState.settled)
			require.Equal(t, before+1, f.store.Generation())
			// Stat-gate identity/size/mtime and tick-12 fallback are pinned by
			// TestOAuthDiskSync04EqualNoPublish in store_oauth_disk_sync_test.go.
			if id != "openai-codex" {
				return
			}
			// Config-local adapter + real Codex provider proves the NEXT send uses
			// adopted credentials; legacy agent transport coverage is separate.
			var requests, rejected atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Authorization") != "Bearer "+replacement.AccessToken || r.Header.Get("chatgpt-account-id") != replacement.AccountID {
					rejected.Add(1)
					http.Error(w, "rejected credentials", http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
			}))
			t.Cleanup(server.Close)
			endpoint, err := url.Parse(server.URL)
			require.NoError(t, err)
			base := server.Client().Transport.(*http.Transport).Clone()
			t.Cleanup(base.CloseIdleConnections)
			client := &http.Client{Timeout: 3 * time.Second, Transport: task1314DiskRoundTripper(func(r *http.Request) (*http.Response, error) {
				forwarded := r.Clone(r.Context())
				forwarded.URL.Scheme, forwarded.URL.Host = endpoint.Scheme, endpoint.Host
				forwarded.Host = endpoint.Host
				return base.RoundTrip(forwarded)
			})}
			provider, err := codexprovider.NewWithSource(client, &task1314DiskSource{store: f.store})
			require.NoError(t, err)
			model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			_, err = model.Generate(ctx, fantasy.Call{})
			require.NoError(t, err)
			require.EqualValues(t, 1, requests.Load())
			require.Zero(t, rejected.Load())
		})
	}
}

func TestTask1314OAuthDiskSyncUncertainRealProviders(t *testing.T) {
	// Revert-check: syncOAuthFromDisk must not clear live credentials on missing, malformed, null, logged-out or unreadable disk data.
	task1314DiskNoExchange(t)
	for _, id := range []string{"openai-codex", "copilot", "hyper"} {
		for _, mode := range []string{"missing", "malformed", "null", "logged-out", "unreadable"} {
			t.Run(id+"/"+mode, func(t *testing.T) {
				f := task1292Fixture(t, id)
				switch mode {
				case "missing":
					require.NoError(t, os.Remove(f.path))
				case "malformed":
					require.NoError(t, os.WriteFile(f.path, []byte("{"), 0o600))
				case "null":
					task1292WriteDisk(t, f.path, id, nil, "")
				case "logged-out":
					require.NoError(t, os.WriteFile(f.path, []byte(`{"providers":{}}`), 0o600))
				case "unreadable":
					require.NoError(t, os.Remove(f.path))
					require.NoError(t, os.Mkdir(f.path, 0o700))
					_, err := os.Stat(f.path)
					require.NoError(t, err, "stat succeeds before the real ReadFile failure")
					_, err = os.ReadFile(f.path)
					require.Error(t, err)
				}
				before := f.store.Generation()
				var state oauthDiskSyncState
				f.store.syncOAuthFromDisk(t.Context(), &state)
				require.Equal(t, f.used.OAuthToken, diskSyncLive(t, f.store, id))
				require.Equal(t, before, f.store.Generation())
			})
		}
	}
}

func TestTask1314OAuthDiskSyncLegacyExpiryAndProviderBoundary(t *testing.T) {
	// Revert-check: legacyDiskTokenAdoptable rejects expired Copilot/Hyper tokens and syncOAuthFromDisk never creates or removes providers or fills nil OAuth entries.
	task1314DiskNoExchange(t)
	for _, id := range []string{"copilot", "hyper"} {
		for _, expired := range []bool{false, true} {
			name := id + "/valid"
			if expired {
				name = id + "/expired"
			}
			t.Run(name, func(t *testing.T) {
				f := task1292Fixture(t, id)
				live := *f.used.OAuthToken
				live.ExpiresAt = time.Now().Add(2 * time.Hour).Unix()
				f.used.OAuthToken = &live
				f.store.SetProviderRuntimeConfig(id, f.used)
				disk := *f.next
				disk.ExpiresAt = time.Now().Add(time.Hour).Unix()
				if expired {
					disk.ExpiresAt = time.Now().Add(-time.Hour).Unix()
				}
				task1292WriteDisk(t, f.path, id, &disk, "")
				before := f.store.Generation()
				var state oauthDiskSyncState
				f.store.syncOAuthFromDisk(t.Context(), &state)
				if expired {
					require.Equal(t, &live, diskSyncLive(t, f.store, id))
					require.Equal(t, before, f.store.Generation())
				} else {
					require.Equal(t, &disk, diskSyncLive(t, f.store, id))
					require.Equal(t, before+1, f.store.Generation())
				}
			})
		}
	}
	for _, id := range []string{"openai-codex", "copilot", "hyper"} {
		t.Run(id+"/provider-boundary", func(t *testing.T) {
			f := task1292Fixture(t, id)
			f.store.SetProviderRuntimeConfig(id, ProviderConfig{ID: id, APIKey: "fixture-static-key"})
			// Keep another real OAuth provider eligible so this reaches the disk loop.
			eligibleID := "copilot"
			if id == eligibleID {
				eligibleID = "hyper"
			}
			f.store.SetProviderRuntimeConfig(eligibleID, ProviderConfig{ID: eligibleID, OAuthToken: f.used.OAuthToken})
			f.store.SetProviderRuntimeConfig("fixture-retained", ProviderConfig{ID: "fixture-retained", APIKey: "fixture-retained-key"})
			data, err := json.Marshal(map[string]any{"providers": map[string]any{id: map[string]any{"oauth": f.next}, "fixture-absent": map[string]any{"oauth": f.next}}})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(f.path, data, 0o600))
			before := f.store.Config().Providers.Copy()
			generation := f.store.Generation()
			var state oauthDiskSyncState
			f.store.syncOAuthFromDisk(t.Context(), &state)
			require.Equal(t, before, f.store.Config().Providers.Copy())
			require.Equal(t, generation, f.store.Generation())
		})
	}
}

func TestTask1314OAuthDiskSyncBurnedCodexPredecessor(t *testing.T) {
	// Revert-check: adoptCodexDiskAuthority and arbitrateOAuthJournal must reject an actual Codex burned predecessor even with a later expiry.
	task1314DiskNoExchange(t)
	f := task1292Fixture(t, "openai-codex")
	burned := *f.used.OAuthToken
	burned.ExpiresAt = f.next.ExpiresAt + 3600
	record := &oauthJournalRecord{Token: f.next, PredecessorRefreshTokenSHA256: oauthRefreshTokenSHA256(f.used.OAuthToken), BurnedRefreshTokenSHA256: []string{oauthRefreshTokenSHA256(&burned)}}
	require.NoError(t, writeOAuthJournalOnce(oauthJournalPath(f.path, f.used.ID), record))
	require.NoError(t, f.store.applyOAuthTokenWithPendingPredecessor(f.used.ID, f.used.OAuthToken, f.next, f.used.OAuthToken, true, record))
	task1292WriteDisk(t, f.path, f.used.ID, &burned, "")
	before := f.store.Generation()
	var state oauthDiskSyncState
	f.store.syncOAuthFromDisk(t.Context(), &state)
	require.Equal(t, f.next, diskSyncLive(t, f.store, f.used.ID))
	require.Equal(t, before, f.store.Generation())
	require.False(t, state.settled)
}
