package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/provider/codexprovider"
	"github.com/PHPCraftdream/rush/internal/agent/provider/nettransport"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/responsesws"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/config"
)

// Published generations fence logout/relogin ABA at the next exchange.
// Unrelated reloads rotate reuse, without revoking an active response.
func (c *coordinator) codexWebSocketOptions() []codexprovider.WebSocketOptions {
	return (&codexWebSocketPolicyManager{host: c}).codexWebSocketOptions()
}

func (c *coordinator) currentCodexWebSocketPolicy() (codexprovider.WebSocketPolicy, config.NetworkConfig, error) {
	return (&codexWebSocketPolicyManager{host: c}).currentCodexWebSocketPolicy()
}

type codexWebSocketPolicyHost interface {
	responsesWSManager() *nettransport.ResponsesWSManager
	responsesWSSnapshot() (*config.Config, uint64)
	responsesWSResolveHTTPClient(*config.Config, config.ProviderConfig) (*http.Client, error)
	responsesWSFallbackOpenaiProvider(baseURL, apiKey string, headers map[string]string, client *http.Client) (fantasy.Provider, error)
}

type codexWebSocketPolicyManager struct {
	host codexWebSocketPolicyHost
}

func (m *codexWebSocketPolicyManager) codexWebSocketOptions() []codexprovider.WebSocketOptions {
	if m.host.responsesWSManager() == nil {
		return nil
	}
	manager := responsesws.NewCodexTransport(m.host.responsesWSManager(), m.currentCodexWebSocketPolicy)
	return []codexprovider.WebSocketOptions{{Manager: manager, EncodeFrame: nettransport.ResponsesWSCodexFullFrame, HTTPClient: func() (*http.Client, error) {
		cfg, _ := m.host.responsesWSSnapshot()
		pc, ok := cfg.Providers.Get(provauth.CodexTokenProviderID)
		if !ok || pc.Disable || pc.OAuthToken == nil {
			return nil, errors.New("openai-codex credentials unavailable")
		}
		return m.host.responsesWSResolveHTTPClient(cfg, pc)
	}, Current: func() (codexprovider.WebSocketPolicy, error) {
		policy, _, err := m.currentCodexWebSocketPolicy()
		return policy, err
	}}}
}

func (m *codexWebSocketPolicyManager) currentCodexWebSocketPolicy() (codexprovider.WebSocketPolicy, config.NetworkConfig, error) {
	cfg, revision := m.host.responsesWSSnapshot()
	pc, ok := cfg.Providers.Get(provauth.CodexTokenProviderID)
	if !ok || pc.Disable || pc.OAuthToken == nil {
		return codexprovider.WebSocketPolicy{}, config.NetworkConfig{}, errors.New("openai-codex credentials unavailable")
	}
	var global *config.NetworkConfig
	var transport config.ResponsesTransport
	if cfg.Options != nil {
		global = cfg.Options.Network
		transport = cfg.Options.ResponsesTransport
	}
	network := nettransport.ResolveNetworkConfig(global, pc.Network)
	encoded, err := json.Marshal(network)
	if err != nil {
		return codexprovider.WebSocketPolicy{}, config.NetworkConfig{}, errors.New("openai-codex invalid network identity")
	}
	digest := sha256.Sum256(encoded)
	policy := codexprovider.WebSocketPolicy{Transport: codexprovider.WebSocketTransport(config.EffectiveResponsesTransport(transport, pc.ResponsesTransport)), NetworkIdentity: hex.EncodeToString(digest[:]), Revision: revision}
	return policy, network, nil
}

func (c *coordinator) responsesWSManager() *nettransport.ResponsesWSManager {
	return c.responsesWS
}

func (c *coordinator) responsesWSSnapshot() (*config.Config, uint64) {
	return c.cfg.Snapshot()
}

func (c *coordinator) responsesWSResolveHTTPClient(cfg *config.Config, providerCfg config.ProviderConfig) (*http.Client, error) {
	return modelkit.ResolveProviderHTTPClient(cfg, providerCfg)
}

func (c *coordinator) responsesWSFallbackOpenaiProvider(baseURL, apiKey string, headers map[string]string, client *http.Client) (fantasy.Provider, error) {
	return modelkit.BuildOpenaiProvider(baseURL, apiKey, headers, client)
}

func (c *coordinator) buildPublicResponsesProvider(pc config.ProviderConfig, baseURL, apiKey string, headers map[string]string, client *http.Client) (fantasy.Provider, error) {
	return (&codexWebSocketPolicyManager{host: c}).buildPublicResponsesProvider(pc, baseURL, apiKey, headers, client)
}

func (m *codexWebSocketPolicyManager) buildPublicResponsesProvider(pc config.ProviderConfig, baseURL, apiKey string, headers map[string]string, client *http.Client) (fantasy.Provider, error) {
	if m.host.responsesWSManager() == nil || pc.ID != "openai" || apiKey == "" || pc.OAuthToken != nil || (baseURL != "" && baseURL != "https://api.openai.com/v1" && baseURL != "https://api.openai.com/v1/") {
		return m.host.responsesWSFallbackOpenaiProvider(baseURL, apiKey, headers, client)
	}
	return responsesws.NewPublicResponsesProvider(baseURL, apiKey, headers, m.host.responsesWSManager(), "", func(ctx context.Context) (responsesws.PublicResponsesPolicy, error) {
		snapshot, revision := m.host.responsesWSSnapshot()
		current, ok := snapshot.Providers.Get(pc.ID)
		if !ok || current.Disable || current.APIKey == "" || current.Type != pc.Type {
			return responsesws.PublicResponsesPolicy{}, errors.New("OpenAI credentials unavailable")
		}
		if current.BaseURL != "" && current.BaseURL != "https://api.openai.com/v1" && current.BaseURL != "https://api.openai.com/v1/" {
			return responsesws.PublicResponsesPolicy{}, errors.New("OpenAI endpoint changed; rebuild provider")
		}
		var global *config.NetworkConfig
		var transport config.ResponsesTransport
		if snapshot.Options != nil {
			global = snapshot.Options.Network
			transport = snapshot.Options.ResponsesTransport
		}
		httpClient, err := m.host.responsesWSResolveHTTPClient(snapshot, current)
		if err != nil {
			return responsesws.PublicResponsesPolicy{}, err
		}
		if httpClient == nil {
			httpClient = http.DefaultClient
		}
		return responsesws.PublicResponsesPolicy{Transport: config.EffectiveResponsesTransport(transport, current.ResponsesTransport), Network: nettransport.ResolveNetworkConfig(global, current.Network), APIKey: current.APIKey, Headers: current.ExtraHeaders, Client: httpClient, Revision: revision}, nil
	}, func(used responsesws.PublicResponsesPolicy) bool {
		snapshot, _ := m.host.responsesWSSnapshot()
		current, ok := snapshot.Providers.Get(pc.ID)
		if !ok || current.Disable || current.Type != pc.Type || current.APIKey != used.APIKey || !maps.Equal(current.ExtraHeaders, used.Headers) {
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
		return config.EffectiveResponsesTransport(transport, current.ResponsesTransport) == used.Transport && nettransport.ResolveNetworkConfig(global, current.Network) == used.Network
	})
}
