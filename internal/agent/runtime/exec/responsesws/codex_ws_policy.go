// Package responsesws codex WebSocket policy manager, adapted by the agent
// root through a host port. The bodies below are the moved ones from
// internal/agent/coordinator_responses_ws.go, unchanged apart from names.
package responsesws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/provider/codexprovider"
	"github.com/PHPCraftdream/rush/internal/agent/provider/nettransport"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
	"github.com/PHPCraftdream/rush/internal/config"
)

type CodexWebSocketPolicyHost interface {
	ResponsesWSManager() *nettransport.ResponsesWSManager
	ResponsesWSSnapshot() (*config.Config, uint64)
	ResponsesWSResolveHTTPClient(*config.Config, config.ProviderConfig) (*http.Client, error)
	ResponsesWSFallbackOpenaiProvider(baseURL, apiKey string, headers map[string]string, client *http.Client) (fantasy.Provider, error)
}

type CodexWebSocketPolicyManager struct {
	host CodexWebSocketPolicyHost
}

func NewCodexWebSocketPolicyManager(host CodexWebSocketPolicyHost) *CodexWebSocketPolicyManager {
	return &CodexWebSocketPolicyManager{host: host}
}

func (m *CodexWebSocketPolicyManager) CodexWebSocketOptions() []codexprovider.WebSocketOptions {
	if m.host.ResponsesWSManager() == nil {
		return nil
	}
	manager := NewCodexTransport(m.host.ResponsesWSManager(), m.CurrentCodexWebSocketPolicy)
	return []codexprovider.WebSocketOptions{{Manager: manager, EncodeFrame: nettransport.ResponsesWSCodexFullFrame, HTTPClient: func() (*http.Client, error) {
		cfg, _ := m.host.ResponsesWSSnapshot()
		pc, ok := cfg.Providers.Get(provauth.CodexTokenProviderID)
		if !ok || pc.Disable || pc.OAuthToken == nil {
			return nil, errors.New("openai-codex credentials unavailable")
		}
		return m.host.ResponsesWSResolveHTTPClient(cfg, pc)
	}, Current: func() (codexprovider.WebSocketPolicy, error) {
		policy, _, err := m.CurrentCodexWebSocketPolicy()
		return policy, err
	}}}
}

func (m *CodexWebSocketPolicyManager) CurrentCodexWebSocketPolicy() (codexprovider.WebSocketPolicy, config.NetworkConfig, error) {
	cfg, revision := m.host.ResponsesWSSnapshot()
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

func (m *CodexWebSocketPolicyManager) BuildPublicResponsesProvider(pc config.ProviderConfig, baseURL, apiKey string, headers map[string]string, client *http.Client) (fantasy.Provider, error) {
	if m.host.ResponsesWSManager() == nil || pc.ID != "openai" || apiKey == "" || pc.OAuthToken != nil || (baseURL != "" && baseURL != "https://api.openai.com/v1" && baseURL != "https://api.openai.com/v1/") {
		return m.host.ResponsesWSFallbackOpenaiProvider(baseURL, apiKey, headers, client)
	}
	return NewPublicResponsesProvider(baseURL, apiKey, headers, m.host.ResponsesWSManager(), "", func(ctx context.Context) (PublicResponsesPolicy, error) {
		snapshot, revision := m.host.ResponsesWSSnapshot()
		current, ok := snapshot.Providers.Get(pc.ID)
		if !ok || current.Disable || current.APIKey == "" || current.Type != pc.Type {
			return PublicResponsesPolicy{}, errors.New("OpenAI credentials unavailable")
		}
		if current.BaseURL != "" && current.BaseURL != "https://api.openai.com/v1" && current.BaseURL != "https://api.openai.com/v1/" {
			return PublicResponsesPolicy{}, errors.New("OpenAI endpoint changed; rebuild provider")
		}
		var global *config.NetworkConfig
		var transport config.ResponsesTransport
		if snapshot.Options != nil {
			global = snapshot.Options.Network
			transport = snapshot.Options.ResponsesTransport
		}
		httpClient, err := m.host.ResponsesWSResolveHTTPClient(snapshot, current)
		if err != nil {
			return PublicResponsesPolicy{}, err
		}
		if httpClient == nil {
			httpClient = http.DefaultClient
		}
		return PublicResponsesPolicy{Transport: config.EffectiveResponsesTransport(transport, current.ResponsesTransport), Network: nettransport.ResolveNetworkConfig(global, current.Network), APIKey: current.APIKey, Headers: current.ExtraHeaders, Client: httpClient, Revision: revision}, nil
	}, func(used PublicResponsesPolicy) bool {
		snapshot, _ := m.host.ResponsesWSSnapshot()
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
