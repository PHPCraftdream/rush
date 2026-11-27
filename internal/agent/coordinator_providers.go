package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

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
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/usage"
	"github.com/PHPCraftdream/rush/internal/agent/safety/notify"
	mcp "github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/config/providers/discover"
	"github.com/PHPCraftdream/rush/internal/kit/sync/event"
	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
)

// Provider client construction, per-provider options, credential refresh,
// and peak-hours policy. Extracted from coordinator.go — pure code move,
// bodies unchanged.

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

func (c *coordinator) withPeakHoursRequestGate(provider fantasy.Provider, pc config.ProviderConfig) fantasy.Provider {
	return usage.NewPeakHoursRequestGateComponent(c).WithPeakHoursRequestGate(provider, pc)
}

func (c *coordinator) CallOptionsFrom(ctx context.Context) *agentcall.CallOptions {
	return callOptionsFrom(ctx)
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

// refreshTokenIfExpired proactively refreshes the OAuth token if it has expired.
func (c *coordinator) refreshTokenIfExpired(ctx context.Context, providerCfg config.ProviderConfig) error {
	_, err := c.refreshExpiredToken(ctx, providerCfg)
	return err
}

// refreshExpiredToken is refreshTokenIfExpired that also says whether a
// refresh really happened: a caller that resolved its model BEFORE the refresh
// holds a client built on the old token and must rebuild it.
func (c *coordinator) refreshExpiredToken(ctx context.Context, providerCfg config.ProviderConfig) (bool, error) {
	if providerCfg.OAuthToken == nil || !providerCfg.OAuthToken.IsExpired() {
		return false, nil
	}
	slog.Debug("Token needs to be refreshed", "provider", providerCfg.ID)
	if err := c.refreshOAuth2Token(ctx, providerCfg); err != nil {
		return false, err
	}
	return true, nil
}

// rebuildOnCurrentCredentials rebuilds model's provider client from the
// current config snapshot, keeping the model itself pinned (same model, new
// client): the pinned/frozen models of a call resolved before a credential
// refresh, and of a delegated child's driver template, carry the old token.
func (c *coordinator) rebuildOnCurrentCredentials(ctx context.Context, model Model, isSubAgent bool) (Model, error) {
	cfg, providerCfg, err := c.rebuildInputs(model.ModelCfg.Provider)
	if err != nil {
		return Model{}, err
	}
	return c.rebuildPinnedModel(ctx, cfg, model, providerCfg, isSubAgent)
}

// publishReauthenticate asks the operator to re-authenticate with providerID
// when it is the hyper provider (the only one with an interactive login flow
// behind the notification).
func (c *coordinator) publishReauthenticate(providerID string) {
	if c.notify == nil || providerID != hyper.Name {
		return
	}
	c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
		Type:       notify.TypeReAuthenticate,
		ProviderID: providerID,
	})
}

// refreshAfterUnauthorized refreshes providerID's credentials after a Drain
// attempt got a 401. It is the ONE place a Drain's 401 refreshes them: the turn
// loop that accounts the attempt calls it before the verdict, because a
// launcher (web wake, delegated child) never sees the error in time and the
// CLI's runInternal returns after the accounting. True means the credentials
// were refreshed (OAuth token or API-key template), so the next attempt can
// succeed and the 401 is a transient failure; false means there is no refresh
// path or it failed, and the 401 stays terminal.
func (c *coordinator) refreshAfterUnauthorized(ctx context.Context, providerID string) bool {
	if c == nil || c.cfg == nil || providerID == "" {
		return false
	}
	_, providerCfg, err := c.rebuildInputs(providerID)
	if err != nil {
		return false
	}
	// The refresh has its own budget, detached from ctx: a slow auth endpoint
	// must not eat what the settle after it needs (R4B-3).
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainRefreshBudget())
	defer cancel()
	if err := c.retryAfterUnauthorized(refreshCtx, providerCfg); err != nil {
		slog.Warn("401 on a Drain: credential refresh skipped", "provider", providerID, "error", err)
		return false
	}
	return true
}

// checkLivePeakHours returns the current peak-hours decision for providerID.
// It reloads the config first when a tracked config file changed on disk, so
// long-running agents in one process can observe peak_hours edits made by the
// web UI or CLI in another process.
func (c *coordinator) checkLivePeakHours(providerID string) error {
	if c == nil || c.cfg == nil || providerID == "" {
		return nil
	}
	if staleness := c.cfg.ConfigStaleness(); staleness.Dirty {
		reloadCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.mcpOwner.ReloadAndReconcileMCPConfig(reloadCtx, c.cfg); err != nil {
			slog.Warn("Failed to reload config before peak-hours check", "provider", providerID, "err", err)
		}
		cancel()
	}
	cfg := c.cfg.Config()
	if cfg == nil || cfg.Providers == nil {
		return nil
	}
	pc, ok := cfg.Providers.Get(providerID)
	if !ok {
		return nil
	}
	return checkPeakHours(pc)
}

func checkPeakHours(providerCfg config.ProviderConfig) error {
	return usage.CheckPeakHours(providerCfg)
}

// runWithUnauthorizedRetry executes fn. If fn returns a 401 error, it
// attempts to refresh credentials and rebuilds the call before retrying.
// Returns the final error: from the retry if a retry was attempted, otherwise
// from the original run. Callers that need to notify the user on persistent
// failure should check isUnauthorized on the returned error.
//
// After credential refresh, rebuildCall is invoked to reconstruct the call
// with fresh credentials, ensuring the retry uses a new provider client
// rather than the stale pinned client from the original attempt (task #341,
// P1-2). A rebuild failure replaces the stale 401 so callers classify the
// terminal local failure correctly.
var errUnauthorizedRefreshUnavailable = errors.New("credential refresh is unavailable")

var errUnauthorizedRebuildUnavailable = errors.New("cannot retry unauthorized request without a call rebuild")

func (c *coordinator) runWithUnauthorizedRetry(ctx context.Context, providerCfg config.ProviderConfig, fn func() error, rebuildCall func() error) error {
	err := fn()
	if err != nil && modelkit.IsUnauthorized(err) {
		retryErr := c.retryAfterUnauthorized(ctx, providerCfg)
		if retryErr != nil {
			slog.Warn("401 retry skipped: credential refresh failed", "provider", providerCfg.ID, "error", retryErr)
			return err
		}
		// Rebuild with the refreshed provider client before retrying.
		if rebuildCall == nil {
			return errUnauthorizedRebuildUnavailable
		}
		if rebuildErr := rebuildCall(); rebuildErr != nil {
			return rebuildErr
		}
		return fn()
	}
	return err
}

// retryAfterUnauthorized attempts to refresh credentials after receiving a 401
// and returns nil if retry should be attempted. This calls UpdateModels which
// clears the model cache and rebuilds the shared agent with fresh credentials
// (task #341, P1-3).
func (c *coordinator) retryAfterUnauthorized(ctx context.Context, providerCfg config.ProviderConfig) error {
	switch {
	case providerCfg.OAuthToken != nil:
		slog.Debug("Received 401. Refreshing token and retrying", "provider", providerCfg.ID)
		return c.refreshOAuth2Token(ctx, providerCfg)
	case strings.Contains(providerCfg.APIKeyTemplate, "$"):
		slog.Debug("Received 401. Refreshing API Key template and retrying", "provider", providerCfg.ID)
		return c.refreshApiKeyTemplate(ctx, providerCfg)
	default:
		return errUnauthorizedRefreshUnavailable
	}
}

func (c *coordinator) refreshOAuth2Token(ctx context.Context, providerCfg config.ProviderConfig) error {
	var err error
	if c.refreshOAuth2TokenFn != nil {
		err = c.refreshOAuth2TokenFn(ctx, providerCfg)
	} else {
		// R5-2: refresh rides the SAME provider network policy (proxy/DNS
		// transport) inference uses, so an auth endpoint reachable only
		// through the configured route cannot split inference (works)
		// from refresh (fails). resolveProviderHTTPClient
		// returns (nil, nil) when NEITHER a proxy/DNS/DoH override NOR
		// debug logging is configured at all — that nil legitimately
		// means "no policy to enforce", and the helpers' historical
		// default-route client is correct for it. A non-nil clientErr is
		// different: policy WAS configured and its build failed, so
		// silently falling back to the default route here would perform
		// the token exchange over an unconfigured route the operator
		// explicitly meant to require (R5-2 residual, 2026-09-22 round-8
		// audit — the earlier "log and fall back" version of this code
		// did exactly that). Refuse instead of downgrading silently.
		// Pair a fresh same-generation provider entry with cfg. A removed
		// provider has no valid network/token pair to refresh.
		cfg, _ := c.cfg.Snapshot()
		if cfg == nil || cfg.Providers == nil {
			return fmt.Errorf("resolve provider network client for OAuth refresh: provider %s is no longer configured; refusing refresh", providerCfg.ID)
		}
		freshProviderCfg, ok := cfg.Providers.Get(providerCfg.ID)
		if !ok {
			return fmt.Errorf("resolve provider network client for OAuth refresh: provider %s is no longer configured; refusing refresh", providerCfg.ID)
		}
		providerCfg = freshProviderCfg
		httpClient, clientErr := modelkit.ResolveProviderHTTPClient(cfg, providerCfg)
		if clientErr != nil {
			return fmt.Errorf("resolve provider network client for OAuth refresh: %w", clientErr)
		}
		// F6 round-12: hand the SAME providerCfg entry the client was
		// built from to the store, so the token it refreshes comes from
		// the exact generation the client came from instead of a fresh,
		// independently-timed store read.
		err = c.cfg.RefreshOAuthTokenWithClient(ctx, config.ScopeGlobal, providerCfg, httpClient)
	}
	if err != nil {
		slog.Error("Failed to refresh OAuth token after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}
	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return nil
}

func (c *coordinator) refreshApiKeyTemplate(ctx context.Context, providerCfg config.ProviderConfig) error {
	newAPIKey, err := c.cfg.Resolve(providerCfg.APIKeyTemplate)
	if err != nil {
		slog.Error("Failed to re-resolve API key after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}

	if !c.cfg.SetProviderRuntimeAPIKeyIfTemplate(providerCfg.ID, providerCfg.APIKeyTemplate, providerCfg.APIKey, newAPIKey) {
		return errUnauthorizedRefreshUnavailable
	}

	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return nil
}

// hasRefreshableCredential reports whether providerCfg's credentials can
// change under a running process: an OAuth token, or an API key resolved from a
// `$` template (retryAfterUnauthorized refreshes exactly these two).
func hasRefreshableCredential(providerCfg config.ProviderConfig) bool {
	return providerCfg.OAuthToken != nil || strings.Contains(providerCfg.APIKeyTemplate, "$")
}

func (c *coordinator) CheckLivePeakHours(providerID string) error {
	return c.checkLivePeakHours(providerID)
}
