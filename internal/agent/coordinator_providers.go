// Provider client construction, per-provider options, credential refresh,
// and peak-hours policy. Extracted from coordinator.go — pure code move,
// bodies unchanged.

package agent

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/provider/codexprovider"
	"github.com/PHPCraftdream/rush/internal/agent/provider/hyper"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/callbeat"
	mcp "github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/config/discover"
	"github.com/PHPCraftdream/rush/internal/kit/sync/event"
)

func validateCredentialProviderID(id string) error {
	return agentcall.ValidateCredentialProviderID(id)
}

func mergeCallOptions(sessionID string, model Model, cfg config.ProviderConfig) (fantasy.ProviderOptions, *float64, *float64, *int64, *float64, *float64) {
	modelOptions := modelkit.GetProviderOptions(sessionID, model, cfg)
	temp := cmp.Or(model.ModelCfg.Temperature, model.CatwalkCfg.Options.Temperature)
	topP := cmp.Or(model.ModelCfg.TopP, model.CatwalkCfg.Options.TopP)
	topK := cmp.Or(model.ModelCfg.TopK, model.CatwalkCfg.Options.TopK)
	freqPenalty := cmp.Or(model.ModelCfg.FrequencyPenalty, model.CatwalkCfg.Options.FrequencyPenalty)
	presPenalty := cmp.Or(model.ModelCfg.PresencePenalty, model.CatwalkCfg.Options.PresencePenalty)
	return modelOptions, temp, topP, topK, freqPenalty, presPenalty
}

// rebuildPinnedModel rebuilds only the provider client for a previously
// selected model. The selected model config and its catalog metadata stay
// pinned; refreshed provider state must not re-resolve the current defaults.
//
// cfg MUST be the SAME snapshot providerCfg was resolved from (round-9 F6
// residual): this rebuild used to receive providerCfg from the caller's
// fetch but take the global network/debug defaults from its own second,
// later c.cfg.Snapshot() — a reload landing between the two reads built
// buildProvider(cfgB, providerCfgA), a provider+network pairing that never
// existed in any single config generation (e.g. generation A's provider
// DNS with generation B's global proxy). Threading the caller's snapshot
// down — the same pattern buildProvider and resolveProviderHTTPClient
// already use — makes the pair atomic by construction. Callers get both
// values from one read via rebuildInputs.
func (c *coordinator) rebuildPinnedModel(ctx context.Context, cfg *config.Config, model Model, providerCfg config.ProviderConfig, isSubAgent bool) (Model, error) {
	if providerCfg.ID == "" {
		return Model{}, errModelProviderNotConfigured
	}

	provider, err := c.buildProvider(cfg, providerCfg, model.ModelCfg, isSubAgent)
	if err != nil {
		return Model{}, err
	}

	modelID := model.ModelCfg.Model
	if providerCfg.ID == openrouter.Name && isExactoSupported(modelID) {
		modelID += ":exacto"
	}
	languageModel, err := provider.LanguageModel(ctx, modelID)
	if err != nil {
		return Model{}, err
	}

	model.Model = languageModel
	model.FlatRate = providerCfg.FlatRate
	return model, nil
}

// rebuildInputs returns the config snapshot and the named provider's config
// FROM THAT SAME snapshot, for the pinned-model rebuild paths (sub-agent
// refresh, summarize refresh). Fetching both from one store read is the
// whole point (round-9 F6 residual): the previous flow read providerCfg via
// currentProviderConfig's snapshot and let rebuildPinnedModel take a
// second, separately-timed snapshot for the globals, so a reload landing
// between the two reads handed the rebuild a mixed cross-generation pair.
func (c *coordinator) rebuildInputs(providerID string) (*config.Config, config.ProviderConfig, error) {
	cfg, _ := c.cfg.Snapshot()
	providerCfg, ok := cfg.Providers.Get(providerID)
	if !ok {
		return nil, config.ProviderConfig{}, errModelProviderNotConfigured
	}
	return cfg, providerCfg, nil
}

func (c *coordinator) currentProviderConfig(providerID string) (config.ProviderConfig, error) {
	cfg, _ := c.cfg.Snapshot()
	providerCfg, ok := cfg.Providers.Get(providerID)
	if !ok {
		return config.ProviderConfig{}, errModelProviderNotConfigured
	}
	return providerCfg, nil
}

// buildProvider takes the pinned cfg snapshot (F6) the caller resolved
// providerCfg from; that SAME snapshot supplies the global network
// defaults resolveProviderHTTPClient composes the HTTP client from.
func (c *coordinator) buildProvider(cfg *config.Config, providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool) (fantasy.Provider, error) {
	if providerCfg.ID == "openai-codex" {
		return c.buildProviderWithValues(cfg, providerCfg, model, isSubAgent, "", "")
	}
	apiKey, _ := c.cfg.Resolve(providerCfg.APIKey)
	baseURL, _ := c.cfg.Resolve(providerCfg.BaseURL)
	return c.buildProviderWithValues(cfg, providerCfg, model, isSubAgent, apiKey, baseURL)
}

// buildProviderWithLiteralCredentials builds a provider from opaque values. It
// skips provider-ID dispatch, OAuth wiring, config resolution, and ambient auth.
func (c *coordinator) buildProviderWithLiteralCredentials(cfg *config.Config, providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool) (fantasy.Provider, error) {
	if providerCfg.Type == bedrock.Name && providerCfg.APIKey == "" {
		return nil, fmt.Errorf("provider %q: api_key is required for per-call Bedrock credentials", providerCfg.ID)
	}
	if providerCfg.Type == "google-vertex" {
		return nil, fmt.Errorf("provider %q: google-vertex credentials are unsupported because Vertex uses ambient credentials", providerCfg.ID)
	}
	providerCfg.ID = ""
	providerCfg.OAuthToken = nil
	return c.buildProviderWithValuesAndAuth(cfg, providerCfg, model, isSubAgent, providerCfg.APIKey, providerCfg.BaseURL, false)
}

// buildProviderWithValues wraps every provider built for agent sessions so
// its model calls land in the heartbeat snapshot.
func (c *coordinator) buildProviderWithValues(cfg *config.Config, providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool, apiKey, baseURL string) (fantasy.Provider, error) {
	return c.buildProviderWithValuesAndAuth(cfg, providerCfg, model, isSubAgent, apiKey, baseURL, true)
}

func (c *coordinator) buildProviderWithValuesAndAuth(cfg *config.Config, providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool, apiKey, baseURL string, allowAmbientAuth bool) (fantasy.Provider, error) {
	p, err := c.buildProviderWithValuesInner(cfg, providerCfg, model, isSubAgent, apiKey, baseURL, allowAmbientAuth)
	if err != nil {
		return nil, err
	}
	return c.withPeakHoursRequestGate(callbeat.NewHeartbeatProvider(p), providerCfg), nil
}

func (c *coordinator) buildProviderWithValuesInner(cfg *config.Config, providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool, apiKey, baseURL string, allowAmbientAuth bool) (fantasy.Provider, error) {
	headers := maps.Clone(providerCfg.ExtraHeaders)
	if headers == nil {
		headers = make(map[string]string)
	}

	// handle special headers for anthropic
	if providerCfg.Type == anthropic.Name && modelkit.IsAnthropicThinking(model) {
		if v, ok := headers["anthropic-beta"]; ok {
			headers["anthropic-beta"] = v + ",interleaved-thinking-2025-05-14"
		} else {
			headers["anthropic-beta"] = "interleaved-thinking-2025-05-14"
		}
	}

	httpClient, err := modelkit.ResolveProviderHTTPClient(cfg, providerCfg)
	if err != nil {
		return nil, err
	}
	httpClient = modelkit.WrapOpenCodeHTTPClient(providerCfg.ID, httpClient)

	switch providerCfg.ID {
	case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
		if modelkit.IsOpenCodeMessagesModel(model.Model) {
			baseURL = strings.TrimSuffix(baseURL, "/v1")
			return modelkit.BuildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID, httpClient)
		}
	}
	if providerCfg.ID == "openai-codex" {
		if c != nil && c.cfg != nil {
			registerCodexVersionPersister(c.cfg)
			return provauth.NewCodexTokenProvider(httpClient, provauth.NewCodexTokenSource(c.cfg, modelkit.ResolveProviderHTTPClient), c.codexWebSocketOptions()...)
		}
		return codexprovider.New(httpClient, providerCfg.OAuthToken)
	}
	httpClient = c.withOAuthBearerClient(providerCfg, httpClient)

	switch providerCfg.Type {
	case openai.Name:
		if allowAmbientAuth {
			return c.buildPublicResponsesProvider(providerCfg, baseURL, apiKey, headers, httpClient)
		}
		return modelkit.BuildOpenaiProvider(baseURL, apiKey, headers, httpClient)
	case anthropic.Name:
		return modelkit.BuildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID, httpClient)
	case openrouter.Name:
		return modelkit.BuildOpenrouterProvider(baseURL, apiKey, headers, httpClient)
	case vercel.Name:
		return modelkit.BuildVercelProvider(baseURL, apiKey, headers, httpClient)
	case azure.Name:
		return modelkit.BuildAzureProvider(baseURL, apiKey, headers, providerCfg.ExtraParams, httpClient)
	case bedrock.Name:
		return modelkit.BuildBedrockProvider(apiKey, headers, httpClient, allowAmbientAuth)
	case google.Name:
		return modelkit.BuildGoogleProvider(baseURL, apiKey, headers, httpClient)
	case "google-vertex":
		return modelkit.BuildGoogleVertexProvider(headers, providerCfg.ExtraParams, httpClient)
	case openaicompat.Name, hyper.Name:
		switch providerCfg.ID {
		case hyper.Name:
			baseURL = hyper.BaseURL() + "/v1"
			headers["x-rush-id"] = event.GetID()
		case string(catwalk.InferenceProviderZAI):
			extraBody := maps.Clone(providerCfg.ExtraBody)
			if extraBody == nil {
				extraBody = make(map[string]any)
			}
			extraBody["tool_stream"] = true
			return modelkit.BuildOpenaiCompatProvider(cfg, baseURL, apiKey, headers, extraBody, providerCfg.ID, isSubAgent, httpClient)
		}
		return modelkit.BuildOpenaiCompatProvider(cfg, baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent, httpClient)
	case cliprovider.ProviderType:
		return cliprovider.New(c.cfg.WorkingDir(), c.cfg.Config().Options.DataDirectory, c.permissions.SkipRequests, c.permissions, c.sessions, &externalMCPProxy{cfg: c.cfg, owner: c.mcpOwner}), nil
	default:
		// Known custom providers (litellm, ollama, omlx, lmstudio) are
		// openai-compat under the hood.
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			return modelkit.BuildOpenaiCompatProvider(cfg, baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent, httpClient)
		}
		return nil, fmt.Errorf("provider type not supported: %q", providerCfg.Type)
	}
}

// externalMCPProxy implements cliprovider.ExternalMCPProxy by delegating to
// the internal mcp package for tool listing and execution.
type externalMCPProxy struct {
	cfg   *config.ConfigStore
	owner *mcp.Owner
}

func (p *externalMCPProxy) ListTools() []cliprovider.ExternalMCPTool {
	var result []cliprovider.ExternalMCPTool
	// Read path stays on the package wrappers: the registry maps are shared
	// name-keyed state; the IsConfigured cfg filter IS the isolation mechanism.
	for serverName, tools := range mcp.Tools() {
		if !mcp.IsConfigured(p.cfg, serverName) {
			continue
		}
		for _, t := range tools {
			result = append(result, cliprovider.ExternalMCPTool{
				ServerName:  serverName,
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			})
		}
	}
	return result
}

func (p *externalMCPProxy) CallTool(ctx context.Context, serverName, toolName, inputJSON string) (string, error) {
	result, err := p.owner.RunTool(ctx, p.cfg, serverName, toolName, inputJSON)
	if err != nil {
		return "", err
	}
	return result.Content, nil
}

func isExactoSupported(modelID string) bool {
	supportedModels := []string{
		"moonshotai/kimi-k2-0905",
		"deepseek/deepseek-v3.1-terminus",
		"z-ai/glm-4.6",
		"openai/gpt-oss-120b",
		"qwen/qwen3-coder",
	}
	return slices.Contains(supportedModels, modelID)
}

// registerCodexVersionPersister stores a raised Codex client version in the global config
// (providers.openai-codex.extra_headers.version). The write is guarded like any settings write:
// under a settings lock it is refused and the raise lasts for this process only.
func registerCodexVersionPersister(store *config.ConfigStore) {
	discover.SetCodexClientVersionPersister(func(from, to string) {
		if err := store.SetConfigField(config.ScopeGlobal, "providers.openai-codex.extra_headers.version", to); err != nil {
			slog.Info("Codex client version raised for this process only", "from", from, "to", to, "error", err)
			return
		}
		slog.Info("Codex client version raised and saved to the config", "from", from, "to", to)
	})
}
