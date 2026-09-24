package agent

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	hyperp "github.com/PHPCraftdream/rush/internal/agent/hyper"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestRefreshOAuth2Token_ProviderRemovedBeforeRefreshSkipsExchange(t *testing.T) {
	var proxyRequests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyRequests.Add(1)
		http.Error(w, "unexpected OAuth exchange", http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)

	providerID := hyperp.Name
	staleProvider := config.ProviderConfig{
		ID:   providerID,
		Type: catwalk.Type(providerID),
		OAuthToken: &oauth.Token{
			AccessToken:  "access-A",
			RefreshToken: "refresh-A",
			ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
		},
	}
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("unrelated-provider", config.ProviderConfig{ID: "unrelated-provider"})
	store := config.NewLibraryStore(&config.Config{
		Options:   &config.Options{Debug: false, Network: &config.NetworkConfig{Proxy: proxy.URL}},
		Providers: providers,
	}, t.TempDir())

	coord := &coordinator{cfg: store}
	err := coord.refreshOAuth2Token(t.Context(), staleProvider)

	require.Error(t, err)
	require.ErrorContains(t, err, "provider "+providerID+" is no longer configured")
	require.Zero(t, proxyRequests.Load(), "removed provider must not send its stale refresh token through the new proxy")
}
