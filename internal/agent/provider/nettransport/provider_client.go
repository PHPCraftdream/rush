package nettransport

import (
	"net/http"

	"github.com/PHPCraftdream/rush/internal/config"
)

// init registers the provider-bound client factory with the config package
// (which cannot import this one). Model-list discovery and verification use
// it so they follow the same proxy cascade as the provider's LLM requests.
func init() {
	config.SetProviderHTTPClientFactory(func(global, provider *config.NetworkConfig) (*http.Client, error) {
		return BuildHTTPClient(ResolveNetworkConfig(global, provider))
	})
}
