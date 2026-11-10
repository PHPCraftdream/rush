package agent

import (
	"context"
	"errors"
	"maps"
	"net/http"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/codexprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/nettransport"
)

func responsesWSReplayForbidden(err error) bool {
	var guard interface{ NoReplay() bool }
	return errors.As(err, &guard) && guard.NoReplay()
}

// Published generations fence logout/relogin ABA at the next exchange.
// Unrelated reloads rotate reuse, without revoking an active response.
func (c *coordinator) codexWebSocketOptions() []codexprovider.WebSocketOptions {
	if c.responsesWS == nil {
		return nil
	}
	return []codexprovider.WebSocketOptions{{Manager: c.responsesWS, HTTPClient: func() (*http.Client, error) {
		cfg, _ := c.cfg.Snapshot()
		pc, ok := cfg.Providers.Get(codexTokenProviderID)
		if !ok || pc.Disable || pc.OAuthToken == nil {
			return nil, errors.New("openai-codex credentials unavailable")
		}
		return c.resolveProviderHTTPClient(cfg, pc)
	}, Current: func() (codexprovider.WebSocketPolicy, error) {
		cfg, revision := c.cfg.Snapshot()
		pc, ok := cfg.Providers.Get(codexTokenProviderID)
		if !ok || pc.Disable || pc.OAuthToken == nil {
			return codexprovider.WebSocketPolicy{}, errors.New("openai-codex credentials unavailable")
		}
		var global *config.NetworkConfig
		var transport config.ResponsesTransport
		if cfg.Options != nil {
			global = cfg.Options.Network
			transport = cfg.Options.ResponsesTransport
		}
		return codexprovider.WebSocketPolicy{Transport: config.EffectiveResponsesTransport(transport, pc.ResponsesTransport), Network: nettransport.ResolveNetworkConfig(global, pc.Network), Revision: revision}, nil
	}}}
}

func (c *coordinator) buildPublicResponsesProvider(pc config.ProviderConfig, baseURL, apiKey string, headers map[string]string, client *http.Client) (fantasy.Provider, error) {
	if c.responsesWS == nil || pc.ID != "openai" || apiKey == "" || pc.OAuthToken != nil || (baseURL != "" && baseURL != "https://api.openai.com/v1" && baseURL != "https://api.openai.com/v1/") {
		return c.buildOpenaiProvider(baseURL, apiKey, headers, client)
	}
	bridge := &publicResponsesClient{manager: c.responsesWS, originalHeaders: headers, current: func(ctx context.Context) (publicResponsesPolicy, error) {
		snapshot, revision := c.cfg.Snapshot()
		current, ok := snapshot.Providers.Get(pc.ID)
		if !ok || current.Disable || current.APIKey == "" || current.Type != pc.Type {
			return publicResponsesPolicy{}, errors.New("OpenAI credentials unavailable")
		}
		if current.BaseURL != "" && current.BaseURL != "https://api.openai.com/v1" && current.BaseURL != "https://api.openai.com/v1/" {
			return publicResponsesPolicy{}, errors.New("OpenAI endpoint changed; rebuild provider")
		}
		var global *config.NetworkConfig
		var transport config.ResponsesTransport
		if snapshot.Options != nil {
			global = snapshot.Options.Network
			transport = snapshot.Options.ResponsesTransport
		}
		httpClient, err := c.resolveProviderHTTPClient(snapshot, current)
		if err != nil {
			return publicResponsesPolicy{}, err
		}
		if httpClient == nil {
			httpClient = http.DefaultClient
		}
		return publicResponsesPolicy{transport: config.EffectiveResponsesTransport(transport, current.ResponsesTransport), network: nettransport.ResolveNetworkConfig(global, current.Network), apiKey: current.APIKey, headers: current.ExtraHeaders, client: httpClient, revision: revision}, nil
	}}
	bridge.valid = func(used publicResponsesPolicy) bool {
		snapshot, _ := c.cfg.Snapshot()
		current, ok := snapshot.Providers.Get(pc.ID)
		if !ok || current.Disable || current.Type != pc.Type || current.APIKey != used.apiKey || !maps.Equal(current.ExtraHeaders, used.headers) {
			return false
		}
		if current.BaseURL != "" && current.BaseURL != "https://api.openai.com/v1" && current.BaseURL != "https://api.openai.com/v1/" {
			return false
		}
		var global *config.NetworkConfig
		var transport config.ResponsesTransport
		if snapshot.Options != nil {
			global = snapshot.Options.Network
			transport = snapshot.Options.ResponsesTransport
		}
		return config.EffectiveResponsesTransport(transport, current.ResponsesTransport) == used.transport && nettransport.ResolveNetworkConfig(global, current.Network) == used.network
	}
	provider, err := newPublicResponsesSDKProvider(baseURL, apiKey, headers, bridge)
	if err != nil {
		return nil, err
	}
	return &publicResponsesProvider{Provider: provider}, nil
}
