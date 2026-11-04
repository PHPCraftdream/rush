package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/stretchr/testify/require"
)

func TestVerifyProviderModelCachesWholeRoster(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "0")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a","display_name":"Model A","context_window":100},{"id":"model-b"},{"id":"model-c"}]}`))
	}))
	defer server.Close()
	pc := ProviderConfig{ID: "custom", Type: "openai", BaseURL: server.URL, APIKey: "test-key"}
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.Providers.Set("custom", pc)
	result, err := VerifyProviderModel(context.Background(), cfg, "custom", "model-a")
	require.NoError(t, err)
	require.Equal(t, Listed, result)
	require.Equal(t, 1, requests)
	models, fresh := CachedConfirmedModels(pc, "custom", IdentityResolver())
	require.True(t, fresh)
	require.Len(t, models, 3)
	require.EqualValues(t, 100, models[0].ContextWindow)
	loaded := mergeConfirmedCatalogModels([]catwalk.Provider{{ID: "custom", Models: []catwalk.Model{{ID: "existing"}}}}, cfg, IdentityResolver())
	require.Len(t, loaded[0].Models, 4)
	require.Equal(t, "model-a", loaded[0].Models[1].ID)
	pc.APIKey = "other"
	models, fresh = CachedConfirmedModels(pc, "custom", IdentityResolver())
	require.False(t, fresh)
	require.Empty(t, models)
	pc.APIKey = "test-key"
	pc.BaseURL = "http://different.invalid"
	models, fresh = CachedConfirmedModels(pc, "custom", IdentityResolver())
	require.False(t, fresh)
	require.Empty(t, models)
	require.NoError(t, ClearModelCatalogCache("all"))
	models, fresh = CachedConfirmedModels(ProviderConfig{ID: "custom", Type: "openai", BaseURL: server.URL, APIKey: "test-key"}, "custom", IdentityResolver())
	require.False(t, fresh)
	require.Empty(t, models)
}

func TestVerifyAnthropicProviderCachesPaginatedRoster(t *testing.T) {
	isolateModelVerifyEnv(t)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "0")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		require.Equal(t, "/v1/models", r.URL.Path)
		if requests == 1 {
			require.Empty(t, r.URL.Query().Get("after_id"))
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a","display_name":"Model A","max_input_tokens":32000,"max_tokens":4096},{"id":"model-b"}],"has_more":true,"last_id":"model-b"}`))
			return
		}
		require.Equal(t, "model-b", r.URL.Query().Get("after_id"))
		_, _ = w.Write([]byte(`{"data":[{"id":"model-c","display_name":"Model C","max_input_tokens":64000,"max_tokens":8192}],"has_more":false}`))
	}))
	defer server.Close()
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.Providers.Set("anthropic", ProviderConfig{ID: "anthropic", Type: "anthropic", BaseURL: server.URL, APIKey: "secret"})
	result, err := VerifyProviderModel(context.Background(), cfg, "anthropic", "model-a")
	require.NoError(t, err)
	require.Equal(t, Listed, result)
	require.Equal(t, 2, requests)
	models, fresh := CachedConfirmedModels(ProviderConfig{Type: "anthropic", BaseURL: server.URL, APIKey: "secret"}, "anthropic", IdentityResolver())
	require.True(t, fresh)
	require.Len(t, models, 3)
	require.Equal(t, "Model C", models[2].Name)
	require.EqualValues(t, 64000, models[2].ContextWindow)
	require.EqualValues(t, 8192, models[2].DefaultMaxTokens)
	result, err = VerifyProviderModel(context.Background(), cfg, "anthropic", "model-b")
	require.NoError(t, err)
	require.Equal(t, Listed, result)
	require.Equal(t, 2, requests, "a second roster model should be found without another request")
}

func TestVerifyProviderModelFreshCacheWithoutModelForcesRefresh(t *testing.T) {
	isolateModelVerifyEnv(t)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "0")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
	}))
	defer server.Close()
	cfg := testVerifyConfig(server.URL, "secret")
	result, err := VerifyProviderModel(context.Background(), cfg, "custom", "model-a")
	require.NoError(t, err)
	require.Equal(t, Listed, result)
	result, err = VerifyProviderModel(context.Background(), cfg, "custom", "model-b")
	require.NoError(t, err)
	require.Equal(t, Listed, result, "a newly released model should appear on the forced refresh")
	require.Equal(t, 2, requests)
	result, err = VerifyProviderModel(context.Background(), cfg, "custom", "model-b")
	require.NoError(t, err)
	require.Equal(t, Listed, result)
	require.Equal(t, 2, requests, "the refreshed roster should now be cached")
}

func TestConfiguredCustomProviderLoadsFreshConfirmedRosterWithoutRequests(t *testing.T) {
	isolateModelVerifyEnv(t)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	models := []catwalk.Model{{ID: "verified-model", Name: "Verified"}}
	require.NoError(t, PersistConfirmedModels("custom", "https://example.invalid", "secret", models))
	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	cfg.Providers.Set("custom", ProviderConfig{ID: "custom", Type: "openai-compat", BaseURL: "https://example.invalid", APIKey: "secret", Models: []catwalk.Model{{ID: "configured"}}})
	providers := []catwalk.Provider{{ID: "custom", Type: "openai-compat", APIEndpoint: "https://example.invalid", APIKey: "secret", Models: []catwalk.Model{{ID: "catalog"}}}}
	e := env.NewFromMap(map[string]string{})
	require.NoError(t, cfg.configureProviders(context.Background(), testStore(cfg), e, NewShellVariableResolver(e), providers))
	loaded, ok := cfg.Providers.Get("custom")
	require.True(t, ok)
	_, known := findCatalogModel(loaded.Models, "verified-model")
	require.True(t, known)
}

func TestVerifyProviderModelHTTPFailuresAreUnverifiable(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			isolateModelVerifyEnv(t)
			t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "0")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "failure", status)
			}))
			defer server.Close()
			cfg := testVerifyConfig(server.URL, "secret")
			result, err := VerifyProviderModel(context.Background(), cfg, "custom", "model")
			require.Error(t, err)
			require.Equal(t, Unverifiable, result)
			models, fresh := CachedConfirmedModels(ProviderConfig{Type: "openai-compat", BaseURL: server.URL, APIKey: "secret"}, "custom", IdentityResolver())
			require.False(t, fresh)
			require.Empty(t, models)
		})
	}
}

func TestVerifyProviderModelCacheOnlyDoesNotRequest(t *testing.T) {
	isolateModelVerifyEnv(t)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	cfg := testVerifyConfig(server.URL, "secret")
	result, err := VerifyProviderModel(context.Background(), cfg, "custom", "model")
	require.Error(t, err)
	require.Equal(t, Unverifiable, result)
	require.Zero(t, requests)
}

func TestVerifyProviderModelTimeoutIsBounded(t *testing.T) {
	isolateModelVerifyEnv(t)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "0")
	previous := modelVerificationTimeout
	modelVerificationTimeout = 25 * time.Millisecond
	t.Cleanup(func() { modelVerificationTimeout = previous })
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	result, err := VerifyProviderModel(context.Background(), testVerifyConfig(server.URL, "secret"), "custom", "model")
	require.Error(t, err)
	require.Equal(t, Unverifiable, result)
}

func TestKnownProviderWithEmptyConfiguredTypeMergesConfirmedModels(t *testing.T) {
	isolateModelVerifyEnv(t)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	require.NoError(t, PersistConfirmedModels("anthropic", "https://api.anthropic.com", "secret", []catwalk.Model{{ID: "future-model"}}))
	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	cfg.Providers.Set("anthropic", ProviderConfig{ID: "anthropic", APIKey: "secret"})
	providers := []catwalk.Provider{{ID: "anthropic", Type: "anthropic", APIEndpoint: "https://api.anthropic.com", APIKey: "secret", Models: []catwalk.Model{{ID: "embedded"}}}}
	e := env.NewFromMap(map[string]string{})
	require.NoError(t, cfg.configureProviders(context.Background(), testStore(cfg), e, NewShellVariableResolver(e), providers))
	loaded, ok := cfg.Providers.Get("anthropic")
	require.True(t, ok)
	require.Contains(t, modelIDs(loaded.Models), "future-model")
}

func TestOrdinaryConfigLoadDoesNotRequestProviderModels(t *testing.T) {
	isolateModelVerifyEnv(t)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "0")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	globalDir := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DIR", globalDir)
	t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(globalDir, "data"))
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(globalDir, "config"))
	globalConfig := filepath.Join(globalDir, "data", "rush.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(globalConfig), 0o700))
	data := []byte(`{"providers":{"anthropic":{"type":"anthropic","base_url":"` + server.URL + `","api_key":"fake-key"}}}`)
	require.NoError(t, os.WriteFile(globalConfig, data, 0o600))
	_, err := Load("", filepath.Join(globalDir, "data"), false)
	require.NoError(t, err)
	require.Zero(t, requests)
}

func isolateModelVerifyEnv(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
}

func testVerifyConfig(endpoint, key string) *Config {
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.Providers.Set("custom", ProviderConfig{ID: "custom", Type: "openai-compat", BaseURL: endpoint, APIKey: key})
	return cfg
}

func TestVerifyProviderModelNoKeyDoesNotRequest(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "0")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.Providers.Set("custom", ProviderConfig{ID: "custom", Type: "openai", BaseURL: server.URL})
	result, err := VerifyProviderModel(context.Background(), cfg, "custom", "x")
	require.Error(t, err)
	require.Equal(t, Unverifiable, result)
	require.Zero(t, requests)
}

func TestClearModelCatalogCacheDropsConfirmedRosterOfThatProvider(t *testing.T) {
	isolateModelVerifyEnv(t)
	models := []catwalk.Model{{ID: "model-a"}}
	require.NoError(t, PersistConfirmedModels("anthropic", "http://e.invalid", "k", models))
	require.NoError(t, PersistConfirmedModels("openai", "http://e.invalid", "k", models))
	require.NotEmpty(t, readConfirmedModels("anthropic", "http://e.invalid", "k"))
	require.NoError(t, ClearModelCatalogCache("anthropic"))
	require.Empty(t, readConfirmedModels("anthropic", "http://e.invalid", "k"))
	require.NotEmpty(t, readConfirmedModels("openai", "http://e.invalid", "k"), "other providers keep their roster")
}

func TestConfiguredCustomProviderDoesNotInheritRosterOfAnotherKeyOrEndpoint(t *testing.T) {
	for _, tc := range []struct{ name, endpoint, key string }{
		{name: "other key", endpoint: "https://example.invalid", key: "other-secret"},
		{name: "other endpoint", endpoint: "https://elsewhere.invalid", key: "secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateModelVerifyEnv(t)
			t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
			require.NoError(t, PersistConfirmedModels("custom", "https://example.invalid", "secret", []catwalk.Model{{ID: "verified-model"}}))
			cfg := &Config{}
			cfg.setDefaults(t.TempDir(), "")
			cfg.Providers.Set("custom", ProviderConfig{ID: "custom", Type: "openai-compat", BaseURL: tc.endpoint, APIKey: tc.key, Models: []catwalk.Model{{ID: "configured"}}})
			providers := []catwalk.Provider{{ID: "custom", Type: "openai-compat", APIEndpoint: tc.endpoint, APIKey: tc.key, Models: []catwalk.Model{{ID: "catalog"}}}}
			e := env.NewFromMap(map[string]string{})
			require.NoError(t, cfg.configureProviders(context.Background(), testStore(cfg), e, NewShellVariableResolver(e), providers))
			loaded, ok := cfg.Providers.Get("custom")
			require.True(t, ok)
			_, known := findCatalogModel(loaded.Models, "verified-model")
			require.False(t, known, "a roster verified with another credential or endpoint must not be trusted")
		})
	}
}
