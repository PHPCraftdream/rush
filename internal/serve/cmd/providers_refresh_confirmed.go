package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
)

func supportsConfirmedProviderRefresh(providerType catwalk.Type) bool {
	switch string(providerType) {
	case "anthropic", "openai", "openai-compatible", "openai_compat", "openai-compat":
		return true
	default:
		return false
	}
}

func refreshConfirmedProviderModels(a *app.App, id string, pc config.ProviderConfig) {
	models, reason, err := config.FetchProviderModels(context.Background(), a.Config(), pc, id, a.Store().Resolver())
	if err != nil || reason != "" {
		if reason == "" {
			reason = "provider request failed"
		}
		fmt.Fprintf(os.Stderr, "note: could not refresh verified model list for %s (%s)\n", id, reason)
		return
	}
	cacheConfirmedProviderModels(a, id, pc, models)
}

func cacheConfirmedProviderModels(a *app.App, id string, pc config.ProviderConfig, models []catwalk.Model) {
	resolver := a.Store().Resolver()
	key, keyErr := resolver.ResolveValue(pc.APIKey)
	endpointValue := pc.BaseURL
	if endpointValue == "" {
		if provider, ok := a.Config().Providers.Get(id); ok {
			endpointValue = provider.BaseURL
		}
	}
	endpoint, endpointErr := resolver.ResolveValue(endpointValue)
	if keyErr != nil || endpointErr != nil || key == "" || endpoint == "" {
		fmt.Fprintf(os.Stderr, "note: could not cache verified model list for %s (provider credentials unavailable)\n", id)
		return
	}
	if err := config.PersistConfirmedModels(id, strings.TrimSpace(endpoint), key, models); err != nil {
		fmt.Fprintf(os.Stderr, "note: could not cache verified model list for %s (cache write failed)\n", id)
	}
}
