package agent

import (
	"fmt"
	"net/http"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/log"
	"github.com/PHPCraftdream/rush/internal/nettransport"
)

// resolveProviderHTTPClient builds the *http.Client this provider's
// outbound connections should use, composing the resolved proxy/DNS/DoH
// network config with the existing Options.Debug request/response
// logging. Returns nil when NEITHER is configured, so callers fall back
// to the provider SDK's own default client unchanged.
func (c *coordinator) resolveProviderHTTPClient(providerCfg config.ProviderConfig) (*http.Client, error) {
	cfg := c.cfg.Config()
	resolved := nettransport.ResolveNetworkConfig(cfg.Options.Network, providerCfg.Network)
	networkClient, err := nettransport.BuildHTTPClient(resolved)
	if err != nil {
		return nil, fmt.Errorf("provider %q: build network client: %w", providerCfg.ID, err)
	}
	if !cfg.Options.Debug {
		return networkClient, nil
	}
	base := http.RoundTripper(http.DefaultTransport)
	if networkClient != nil {
		base = networkClient.Transport
	}
	return log.NewHTTPClientWithTransport(base), nil
}

// copilotBaseTransport extracts the base round tripper from the optional
// resolved HTTP client so the Copilot initiator transport can layer on top
// of the proxy/DNS-configured transport. Nil means "nothing configured".
func copilotBaseTransport(httpClient *http.Client) http.RoundTripper {
	if httpClient == nil {
		return nil
	}
	return httpClient.Transport
}
