package agent

import (
	"net/http"

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
	return responsesws.NewCodexWebSocketPolicyManager(coordinatorCodexWebSocketPolicyHost{coordinator: c}).CodexWebSocketOptions()
}

func (c *coordinator) currentCodexWebSocketPolicy() (codexprovider.WebSocketPolicy, config.NetworkConfig, error) {
	return responsesws.NewCodexWebSocketPolicyManager(coordinatorCodexWebSocketPolicyHost{coordinator: c}).CurrentCodexWebSocketPolicy()
}

// coordinatorCodexWebSocketPolicyHost is the root-side port adapter: the
// coordinator surface the codex WebSocket policy manager needs, exported
// only because the destination package has to see it.
type coordinatorCodexWebSocketPolicyHost struct {
	coordinator *coordinator
}

func (h coordinatorCodexWebSocketPolicyHost) ResponsesWSManager() *nettransport.ResponsesWSManager {
	return h.coordinator.responsesWSManager()
}

func (h coordinatorCodexWebSocketPolicyHost) ResponsesWSSnapshot() (*config.Config, uint64) {
	return h.coordinator.responsesWSSnapshot()
}

func (h coordinatorCodexWebSocketPolicyHost) ResponsesWSResolveHTTPClient(cfg *config.Config, providerCfg config.ProviderConfig) (*http.Client, error) {
	return h.coordinator.responsesWSResolveHTTPClient(cfg, providerCfg)
}

func (h coordinatorCodexWebSocketPolicyHost) ResponsesWSFallbackOpenaiProvider(baseURL, apiKey string, headers map[string]string, client *http.Client) (fantasy.Provider, error) {
	return h.coordinator.responsesWSFallbackOpenaiProvider(baseURL, apiKey, headers, client)
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
	return responsesws.NewCodexWebSocketPolicyManager(coordinatorCodexWebSocketPolicyHost{coordinator: c}).BuildPublicResponsesProvider(pc, baseURL, apiKey, headers, client)
}
