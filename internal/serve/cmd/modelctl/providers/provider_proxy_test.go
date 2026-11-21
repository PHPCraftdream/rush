package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"

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

const proxiedRoster = `{"data":[{"id":"model-x"},{"id":"other"}]}`

func TestProvidersUpdateFollowsTheProxyCascade(t *testing.T) {
	t.Cleanup(config.SetModelVerifyOnlineForTests(true))
	roster := `{"data":[{"id":"future-model","context_window":64000}]}`

	t.Run("global proxy", func(t *testing.T) {
		proxy := newRosterProxy(t, roster)
		fixture := `{"options":{"network":{"proxy":"` + proxy.URL() + `"}},"providers":{"custom":{"name":"Custom","type":"openai-compat","base_url":"` + unreachableProviderBase + `","api_key":"test-key","models":[{"id":"seed-model"}]}}}`
		_, _, _, err := cmdtest.RunProvidersCmdInIsolatedAppFull(t, providersUpdateCmd, fixture, "custom", "")
		require.NoError(t, err)
		require.Contains(t, proxy.seen(), "models.invalid")
		ids, fresh := confirmedCustom(unreachableProviderBase)
		require.True(t, fresh)
		require.Contains(t, ids, "future-model")
	})

	t.Run("provider none does not use the global proxy", func(t *testing.T) {
		proxy := newRosterProxy(t, roster)
		fixture := `{"options":{"network":{"proxy":"` + proxy.URL() + `"}},"providers":{"custom":{"name":"Custom","type":"openai-compat","base_url":"` + unreachableProviderBase + `","api_key":"test-key","network":{"proxy":"none"},"models":[{"id":"seed-model"}]}}}`
		_, _, _, err := cmdtest.RunProvidersCmdInIsolatedAppFull(t, providersUpdateCmd, fixture, "custom", "")
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
