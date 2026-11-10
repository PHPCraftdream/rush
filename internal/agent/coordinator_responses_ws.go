package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	manager := &codexWebSocketTransport{ResponsesWSManager: c.responsesWS, current: c.currentCodexWebSocketPolicy}
	return []codexprovider.WebSocketOptions{{Manager: manager, EncodeFrame: nettransport.ResponsesWSCodexFullFrame, HTTPClient: func() (*http.Client, error) {
		cfg, _ := c.cfg.Snapshot()
		pc, ok := cfg.Providers.Get(codexTokenProviderID)
		if !ok || pc.Disable || pc.OAuthToken == nil {
			return nil, errors.New("openai-codex credentials unavailable")
		}
		return c.resolveProviderHTTPClient(cfg, pc)
	}, Current: func() (codexprovider.WebSocketPolicy, error) {
		policy, _, err := c.currentCodexWebSocketPolicy()
		return policy, err
	}}}
}

func (c *coordinator) currentCodexWebSocketPolicy() (codexprovider.WebSocketPolicy, config.NetworkConfig, error) {
	cfg, revision := c.cfg.Snapshot()
	pc, ok := cfg.Providers.Get(codexTokenProviderID)
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

type codexWebSocketTransport struct {
	*nettransport.ResponsesWSManager
	current func() (codexprovider.WebSocketPolicy, config.NetworkConfig, error)
}

func (t *codexWebSocketTransport) Acquire(ctx context.Context, request codexprovider.WebSocketRequest) (codexprovider.WebSocketLease, error) {
	policy, network, err := t.current()
	if err != nil {
		return nil, err
	}
	if policy != request.Policy {
		return nil, errors.New("openai-codex connection policy changed before handshake")
	}
	lease, err := t.ResponsesWSManager.Acquire(ctx, nettransport.ResponsesWSOptions{
		Protocol: "codex-responses", Endpoint: request.Endpoint, ReadPolicy: nettransport.ResponsesWSReadRequestContext,
		AuthToken: request.AuthToken, Headers: request.Headers, Network: network, StillCurrent: request.StillCurrent,
		Owner: nettransport.ResponsesWSOwner{AccountID: request.AccountID, AuthRevision: request.AuthRevision, ConversationID: request.ConversationID},
	}, nettransport.ResponsesWSPlannedMaxAge)
	if err != nil {
		return nil, err
	}
	return codexWebSocketLease{lease}, nil
}

func (*codexWebSocketTransport) UnauthorizedHandshake(err error) bool {
	var handshake *nettransport.ResponsesWSHandshakeError
	return errors.As(err, &handshake) && handshake.Status == http.StatusUnauthorized
}

type codexWebSocketLease struct{ *nettransport.ResponsesWSLease }

func (l codexWebSocketLease) Conn() codexprovider.WebSocketConnection {
	return l.ResponsesWSLease.Conn()
}

var (
	_ codexprovider.WebSocketManager       = (*codexWebSocketTransport)(nil)
	_ codexprovider.WebSocketExchangeError = (*nettransport.ResponsesWSError)(nil)
)

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
