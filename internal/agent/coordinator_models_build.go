package agent

import (
	"context"
	"log/slog"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openrouter"

	"github.com/PHPCraftdream/rush/internal/config"
)

// TODO: when we support multiple agents we need to change this so that we pass in the agent specific model config
func (c *coordinator) buildAgentModels(ctx context.Context, isSubAgent bool) (Model, Model, error) {
	// Single atomic snapshot for every config read below (task #341/P1-3;
	// gap found by independent review — this function used to read
	// smart/fast/worker each via a separate c.cfg.Config() call, then take
	// a fourth, separate Snapshot() just for buildModelsFromCfg's provider
	// lookups. A reload landing between any of those reads could produce a
	// cross-generation mix, exactly what Snapshot() exists to prevent.
	cfg, _ := c.cfg.Snapshot()
	return c.buildAgentModelsFromCfg(ctx, cfg, isSubAgent)
}

// buildAgentModelsFromCfg is buildAgentModels' body, taking the config
// snapshot as a parameter instead of capturing its own (task #576/P1-3).
// buildAgent (coordinator_tools.go) pins ONE *config.Config for its entire
// build -- models, prompt, and tools -- and passes it here so the models it
// builds always agree with the prompt/toolset built from the same call. A
// caller with nothing to pin (buildAgentModels above) takes its own fresh
// Snapshot() and delegates here, so behavior for those callers is unchanged.
func (c *coordinator) buildAgentModelsFromCfg(ctx context.Context, cfg *config.Config, isSubAgent bool) (Model, Model, error) {
	if creds := callCredentialsFrom(ctx); creds != nil {
		choice := creds.Models[RoleSmart]
		if isSubAgent {
			// A worker choice is the only tenant worker slot. When it is
			// omitted, dispatch deliberately falls back to tenant smart.
			if workerChoice, ok := creds.Models[RoleWorker]; ok {
				choice = workerChoice
			}
		}
		smart, _, err := c.buildCredentialModel(ctx, cfg, creds, choice)
		if err != nil {
			return Model{}, Model{}, err
		}
		fast := smart
		if fastChoice, ok := creds.Models[RoleFast]; ok {
			fast, _, err = c.buildCredentialModel(ctx, cfg, creds, fastChoice)
			if err != nil {
				return Model{}, Model{}, err
			}
		}
		return smart, fast, nil
	}
	smartModelCfg, ok := cfg.Models[config.SelectedModelTypeSmart]
	if !ok {
		return Model{}, Model{}, errSmartModelNotSelected
	}
	fastModelCfg, ok := cfg.Models[config.SelectedModelTypeFast]
	if !ok {
		return Model{}, Model{}, errFastModelNotSelected
	}

	// Fork patch (reviewer/worker roles): when spawning a sub-agent acting as
	// a worker (see workerSubAgentActive), prefer the cheaper Worker slot for
	// the sub-agent's smart-model slot. This never touches the fast-model
	// slot, and falls through to today's behavior (Smart for everything)
	// otherwise.
	if isSubAgent && c.workerSubAgentActiveForCall(ctx, cfg) {
		smartModelCfg = cfg.Models[config.SelectedModelTypeWorker]
	}

	return c.buildModelsFromCfg(ctx, cfg, smartModelCfg, fastModelCfg, isSubAgent)
}

// buildModelsFromCfg builds Model objects from explicit SelectedModel configs.
// The cfg parameter must be from a single atomic Snapshot() call to ensure
// consistency across all provider reads (task #341, P1-3).
func (c *coordinator) buildModelsFromCfg(ctx context.Context, cfg *config.Config, smartModelCfg, fastModelCfg config.SelectedModel, isSubAgent bool) (Model, Model, error) {
	smartProviderCfg, ok := cfg.Providers.Get(smartModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errSmartModelProviderNotConfigured
	}

	smartProvider, err := c.buildProvider(cfg, smartProviderCfg, smartModelCfg, isSubAgent)
	if err != nil {
		return Model{}, Model{}, err
	}

	fastProviderCfg, ok := cfg.Providers.Get(fastModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errFastModelProviderNotConfigured
	}

	fastProvider, err := c.buildProvider(cfg, fastProviderCfg, fastModelCfg, true)
	if err != nil {
		return Model{}, Model{}, err
	}

	var smartCatwalkModel *catwalk.Model
	var fastCatwalkModel *catwalk.Model

	for _, m := range smartProviderCfg.Models {
		if m.ID == smartModelCfg.Model {
			smartCatwalkModel = &m
		}
	}
	for _, m := range fastProviderCfg.Models {
		if m.ID == fastModelCfg.Model {
			fastCatwalkModel = &m
		}
	}

	// Unverified model: the operator's config names a model id this
	// provider's cached catalog doesn't know about (see findModels' doc
	// comment in internal/app/provider.go — `rush models use`/`rush run
	// --model` accept this explicitly, with a warning, for a just-announced
	// model the catalog hasn't synced yet). Synthesize a minimal catwalk.Model
	// (zero-value cost/context-window/reasoning fields) rather than refusing
	// to build the agent at all — every CatwalkCfg reader downstream already
	// treats a zero ContextWindow/CostPer1M*/CanReason as "unknown, don't
	// assume" (see agent_turn.go's `if cw > 0` gate before any context-window
	// budget logic runs), so this degrades to "no proactive trimming, cost
	// shows as free/unknown, no reasoning params sent" instead of crashing or
	// refusing to start.
	if smartCatwalkModel == nil {
		slog.Warn("smart model not found in provider's known catalog; using unverified minimal metadata (cost/context-window unknown)",
			"provider", smartModelCfg.Provider, "model", smartModelCfg.Model)
		smartCatwalkModel = &catwalk.Model{ID: smartModelCfg.Model, Name: smartModelCfg.Model}
	}

	if fastCatwalkModel == nil {
		slog.Warn("fast model not found in provider's known catalog; using unverified minimal metadata (cost/context-window unknown)",
			"provider", fastModelCfg.Provider, "model", fastModelCfg.Model)
		fastCatwalkModel = &catwalk.Model{ID: fastModelCfg.Model, Name: fastModelCfg.Model}
	}

	smartModelID := smartModelCfg.Model
	fastModelID := fastModelCfg.Model

	if smartModelCfg.Provider == openrouter.Name && isExactoSupported(smartModelID) {
		smartModelID += ":exacto"
	}

	if fastModelCfg.Provider == openrouter.Name && isExactoSupported(fastModelID) {
		fastModelID += ":exacto"
	}

	smartModel, err := smartProvider.LanguageModel(ctx, smartModelID)
	if err != nil {
		return Model{}, Model{}, err
	}
	fastModel, err := fastProvider.LanguageModel(ctx, fastModelID)
	if err != nil {
		return Model{}, Model{}, err
	}

	smart := Model{
		Model:      smartModel,
		CatwalkCfg: *smartCatwalkModel,
		ModelCfg:   smartModelCfg,
		FlatRate:   smartProviderCfg.FlatRate,
	}
	fast := Model{
		Model:      fastModel,
		CatwalkCfg: *fastCatwalkModel,
		ModelCfg:   fastModelCfg,
		FlatRate:   fastProviderCfg.FlatRate,
	}
	return smart, fast, nil
}

// Model returns the globally configured smart model from config.
// This is used for display/status queries and does NOT reflect any per-session
// model overrides. After the per-session model isolation fix, callers that need
// the actual model for a specific session should use resolveSessionModels instead.
func (c *coordinator) Model() Model {
	// Build the default smart model from config without caching (this is
	// called infrequently, mostly for status display).
	cfg, _ := c.cfg.Snapshot()
	smartCfg := cfg.Models[config.SelectedModelTypeSmart]
	fastCfg := cfg.Models[config.SelectedModelTypeFast]

	// Create a temporary context for model building.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	smartModel, _, err := c.buildModelsFromCfg(ctx, cfg, smartCfg, fastCfg, false)
	if err != nil {
		// Return a zero-value model rather than panicking on status queries.
		slog.Error("coordinator.Model: failed to build default smart model", "err", err)
		return Model{}
	}
	return smartModel
}

// updateModelsReadyGateSeam is a test hook after registration and before the
// local drain, used to force cancellation/failure without affecting admission.
var updateModelsReadyGateSeam func(context.Context, *readyGate)

// UpdateModels refreshes the coordinator-wide default agent (models, prompt
// prefix, toolset) from the LATEST config: config reloads (SetupAgents) and
// credential refreshes use it to republish clients and tools — including
// live MCP tool-list changes — for LEGACY callers whose turns still read the
// shared state. It is NOT a per-run hook (R3-1): any CallOptions carried by
// ctx are stripped first, so a single run's DisableSubAgents/ModelRole can
// never decide what every other in-flight turn sees. Per-call toolsets are
// built and pinned by resolveSessionModels/applyModelOverrides instead.
func (c *coordinator) UpdateModels(ctx context.Context) error {
	ctx = withoutCallOptions(ctx)
	c.clearModelCache()

	// build the models again so we make sure we get the latest config
	cfg, _ := c.cfg.Snapshot()
	smart, fast, err := c.buildAgentModelsFromCfg(ctx, cfg, false)
	if err != nil {
		return err
	}
	agentCfg, ok := cfg.Agents[config.AgentCoder]
	if !ok {
		return errCoderAgentNotConfigured
	}
	// The live agent is only replaced after this rebuild has completed cleanly.
	buildGate := &readyGate{}
	ctx = withReadyGate(ctx, buildGate)
	tools, err := c.buildTools(ctx, cfg, agentCfg, false)
	if err != nil {
		_ = buildGate.Wait()
		return err
	}
	if updateModelsReadyGateSeam != nil {
		updateModelsReadyGateSeam(ctx, buildGate)
	}
	if err := buildGate.Wait(); err != nil {
		return err
	}
	c.currentAgent.SetModels(smart, fast)
	if smartProviderCfg, ok := cfg.Providers.Get(smart.ModelCfg.Provider); ok {
		c.currentAgent.SetSystemPromptPrefix(smartProviderCfg.SystemPromptPrefix)
	}
	c.currentAgent.SetTools(tools)
	return nil
}

// clearModelCache empties the model cache, forcing the next resolveSessionModels
// call to rebuild models from the current config. This is called after credential
// updates (OAuth token refresh, API key re-resolution) to ensure cached models
// with stale clients are not reused (task #341, P1-3).
func (c *coordinator) clearModelCache() {
	c.modelCacheMu.Lock()
	defer c.modelCacheMu.Unlock()

	c.modelCacheEpoch++
	if c.modelCache == nil {
		return
	}
	if cache, ok := c.modelCache.(*boundedModelPairCache); ok {
		cache.clear()
		return
	}
	if cache, ok := c.modelCache.(interface {
		Reset(map[string]cachedModelPair)
	}); ok {
		cache.Reset(make(map[string]cachedModelPair))
	}
}
