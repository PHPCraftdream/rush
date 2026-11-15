package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// unreachableProviderBase cannot resolve in DNS, so a request reaches the
// test only by travelling through a proxy double that answers it as origin.
const unreachableProviderBase = "http://models.invalid/v1"

type rosterProxy struct {
	server *httptest.Server
	mu     sync.Mutex
	hosts  []string
}

func newRosterProxy(t *testing.T, body string) *rosterProxy {
	t.Helper()
	p := &rosterProxy{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.hosts = append(p.hosts, r.Host)
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *rosterProxy) URL() string { return p.server.URL }

func (p *rosterProxy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.hosts...)
}

// seedProxiedProvider writes a custom openai-compat provider that can only be
// reached through a proxy, plus the given global and provider network blocks.
func seedProxiedProvider(t *testing.T, globalPath string, globalNetwork, providerNetwork map[string]any) {
	t.Helper()
	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, json.Unmarshal(data, &cfg))
	provider := map[string]any{
		"type": "openai-compat", "base_url": unreachableProviderBase, "api_key": "test-key",
		"models": []map[string]string{{"id": "seed-model", "name": "Seed Model"}},
	}
	if providerNetwork != nil {
		provider["network"] = providerNetwork
	}
	cfg["providers"].(map[string]any)["custom"] = provider
	if globalNetwork != nil {
		options, _ := cfg["options"].(map[string]any)
		if options == nil {
			options = map[string]any{}
		}
		options["network"] = globalNetwork
		cfg["options"] = options
	}
	data, err = json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(globalPath, data, 0o600))
}

func useFastModel(t *testing.T) (stderr string, err error) {
	t.Helper()
	resetModelsUseFlags(t)
	stderr = captureStderr(t, func() { _, err = runModelsCmd(t, modelsUseCmd, "--fast", "custom/model-x") })
	return stderr, err
}

const proxiedRoster = `{"data":[{"id":"model-x"},{"id":"other"}]}`

func TestModelsUseVerificationFollowsTheProxyCascade(t *testing.T) {
	t.Run("global proxy carries the verification request", func(t *testing.T) {
		globalPath := verifyEnv(t, true)
		global := newRosterProxy(t, proxiedRoster)
		seedProxiedProvider(t, globalPath, map[string]any{"proxy": global.URL()}, nil)

		stderr, err := useFastModel(t)
		require.NoError(t, err)
		require.Contains(t, stderr, "note: fast model custom/model-x is set")
		require.NotContains(t, stderr, "warning:")
		require.Contains(t, global.seen(), "models.invalid")
	})

	t.Run("provider proxy replaces the global proxy", func(t *testing.T) {
		globalPath := verifyEnv(t, true)
		global := newRosterProxy(t, `{"data":[]}`)
		provider := newRosterProxy(t, proxiedRoster)
		seedProxiedProvider(t, globalPath, map[string]any{"proxy": global.URL()}, map[string]any{"proxy": provider.URL()})

		stderr, err := useFastModel(t)
		require.NoError(t, err)
		require.Contains(t, stderr, "note: fast model custom/model-x is set")
		require.Contains(t, provider.seen(), "models.invalid")
		require.NotContains(t, global.seen(), "models.invalid", "the global proxy must not see a provider that overrides it")
	})

	t.Run("provider none bypasses the global proxy", func(t *testing.T) {
		globalPath := verifyEnv(t, true)
		global := newRosterProxy(t, proxiedRoster)
		seedProxiedProvider(t, globalPath, map[string]any{"proxy": global.URL()}, map[string]any{"proxy": "none"})

		stderr, err := useFastModel(t)
		require.NoError(t, err)
		require.Contains(t, stderr, "could not be verified", "models.invalid is unreachable without the proxy")
		require.NotContains(t, global.seen(), "models.invalid", "proxy: none must keep the request off the global proxy")
		require.Contains(t, savedFastModel(t, globalPath), `"model-x"`, "the model is still set")
	})

	t.Run("malformed provider proxy fails loudly instead of going direct", func(t *testing.T) {
		globalPath := verifyEnv(t, true)
		global := newRosterProxy(t, proxiedRoster)
		seedProxiedProvider(t, globalPath, map[string]any{"proxy": global.URL()}, map[string]any{"proxy": "ftp://proxy.example:21"})

		stderr, err := useFastModel(t)
		require.NoError(t, err)
		require.Contains(t, stderr, "provider network settings are invalid")
		require.NotContains(t, global.seen(), "models.invalid")
	})
}

func TestProvidersUpdateFollowsTheProxyCascade(t *testing.T) {
	t.Cleanup(config.SetModelVerifyOnlineForTests(true))
	roster := `{"data":[{"id":"future-model","context_window":64000}]}`

	t.Run("global proxy", func(t *testing.T) {
		proxy := newRosterProxy(t, roster)
		fixture := `{"options":{"network":{"proxy":"` + proxy.URL() + `"}},"providers":{"custom":{"name":"Custom","type":"openai-compat","base_url":"` + unreachableProviderBase + `","api_key":"test-key","models":[{"id":"seed-model"}]}}}`
		_, globalPath, _, err := runProvidersCmdInIsolatedAppFull(t, providersUpdateCmd, fixture, "custom", "")
		require.NoError(t, err)
		require.Contains(t, proxy.seen(), "models.invalid")
		ids, fresh := confirmedCustom(globalPath, unreachableProviderBase)
		require.True(t, fresh)
		require.Contains(t, ids, "future-model")
	})

	t.Run("provider none does not use the global proxy", func(t *testing.T) {
		proxy := newRosterProxy(t, roster)
		fixture := `{"options":{"network":{"proxy":"` + proxy.URL() + `"}},"providers":{"custom":{"name":"Custom","type":"openai-compat","base_url":"` + unreachableProviderBase + `","api_key":"test-key","network":{"proxy":"none"},"models":[{"id":"seed-model"}]}}}`
		_, _, _, err := runProvidersCmdInIsolatedAppFull(t, providersUpdateCmd, fixture, "custom", "")
		require.Error(t, err, "the unreachable host cannot be fetched without the proxy")
		require.Empty(t, proxy.seen())
	})
}

func TestPingProviderFollowsTheProxyCascade(t *testing.T) {
	proxy := newRosterProxy(t, proxiedRoster)
	pc := config.ProviderConfig{
		ID: "custom", Type: "openai-compat", BaseURL: unreachableProviderBase, APIKey: "test-key",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg := &config.Config{Options: &config.Options{Network: &config.NetworkConfig{Proxy: proxy.URL()}}}
	res := pingProvider(ctx, cfg, "custom", pc)
	require.Equal(t, "ok", res.Status, "ping result: %+v", res)
	require.Equal(t, 2, res.ModelsFound)
	require.Contains(t, proxy.seen(), "models.invalid")

	hits := len(proxy.seen())
	pc.Network = &config.NetworkConfig{Proxy: "none"}
	res = pingProvider(ctx, cfg, "custom", pc)
	require.Equal(t, "unreachable", res.Status, "ping result: %+v", res)
	require.Len(t, proxy.seen(), hits, "proxy: none must not reach the global proxy")
}
