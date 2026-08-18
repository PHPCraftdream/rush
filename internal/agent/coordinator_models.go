// Per-session model resolution: override snapshots, pinning, and the
// model-pair cache. Extracted from coordinator.go — pure code move,
// bodies unchanged.

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openrouter"
	"github.com/charmbracelet/crush/internal/config"
)

// resolvedOverrides is what applyModelOverrides computed, captured so the
// caller can pin it onto the SessionAgentCall (task #265, P0-1).
//
// Without this, the sequence was: applyModelOverrides writes the resolved
// values into the SHARED agent, then runInternal reads them back out of that
// same shared agent a few dozen lines later, then the turn reads them AGAIN
// when it actually starts. Every one of those gaps is a window where another
// session's applyModelOverrides can land, and this fork's whole premise is N
// concurrent sessions. Returning the values means the caller never has to
// read them back.
type resolvedOverrides struct {
	large        Model
	small        Model
	promptPrefix string
	systemPrompt string
	// providerCfg is the large model's provider config, resolved from the
	// SAME Snapshot()/Config() call that built `large` above. Callers that
	// need provider options/credentials for this same model later in one
	// logical resolve/build operation (runInternal's 401 rebuildCall) MUST
	// read this field instead of taking their own, separately-timed
	// snapshot — otherwise a reload landing between "model resolved" and
	// "provider options computed" can mix a model from one config
	// generation with credentials/options from another (task #341, P1-1).
	// Zero-value (config.ProviderConfig{}) when the resolving path never
	// populated it (e.g. applyModelOverrides callers that don't need it);
	// always populated by resolveSessionModels.
	providerCfg config.ProviderConfig
}

// resolveSessionModels builds an immutable snapshot of model configuration for a session.
// It reads from the session DB if overrides are present, otherwise falls back to
// the global config defaults. This method NEVER writes to shared state (c.currentAgent),
// ensuring that per-session model choices don't affect other concurrent sessions.
//
// The returned snapshot includes both large and small models, the provider's system
// prompt prefix, and the built system prompt (if a prompt template is available).
//
// Results are cached per unique (config generation, provider+model+reasoning_effort) pair.
// The config generation is included so that any config change (reload, credential update,
// etc.) invalidates the cache, preventing stale clients from being reused (task #341, P1-3).
//
// All config reads use a single atomic Snapshot() call to prevent reading config fields
// from different generations (task #341, P1-3).
func (c *coordinator) resolveSessionModels(ctx context.Context, sessionID string) (*resolvedOverrides, error) {
	// Load the session to check for model overrides.
	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to load session %q: %w", sessionID, err)
	}

	// Atomically capture config and generation in a single snapshot to prevent
	// torn reads across reloads (task #341, P1-3).
	cfg, gen := c.cfg.Snapshot()

	// Start with the global config defaults.
	largeCfg := cfg.Models[config.SelectedModelTypeLarge]
	smallCfg := cfg.Models[config.SelectedModelTypeSmall]

	// Apply session-level overrides from the DB if present.
	var largeOverride, smallOverride *ModelOverride
	if sess.LargeModelID != "" {
		largeOverride = &ModelOverride{
			Provider:        sess.LargeModelProvider,
			Model:           sess.LargeModelID,
			ReasoningEffort: sess.LargeModelReasoningEffort,
		}
	}
	if sess.SmallModelID != "" {
		smallOverride = &ModelOverride{
			Provider:        sess.SmallModelProvider,
			Model:           sess.SmallModelID,
			ReasoningEffort: sess.SmallModelReasoningEffort,
		}
	}

	// Merge overrides into the config copies.
	if largeOverride != nil {
		if largeCfg.Provider != largeOverride.Provider || largeCfg.Model != largeOverride.Model {
			largeCfg.Think = false
			largeCfg.ReasoningEffort = ""
		}
		largeCfg.Provider = largeOverride.Provider
		largeCfg.Model = largeOverride.Model
		if largeOverride.ReasoningEffort != "" {
			largeCfg.ReasoningEffort = largeOverride.ReasoningEffort
		}
	}
	if smallOverride != nil {
		if smallCfg.Provider != smallOverride.Provider || smallCfg.Model != smallOverride.Model {
			smallCfg.Think = false
			smallCfg.ReasoningEffort = ""
		}
		smallCfg.Provider = smallOverride.Provider
		smallCfg.Model = smallOverride.Model
		if smallOverride.ReasoningEffort != "" {
			smallCfg.ReasoningEffort = smallOverride.ReasoningEffort
		}
	}

	// Build (or reuse from cache) both models TOGETHER in a single
	// buildModelsFromCfg call, keyed by the combined large+small
	// provider+model+reasoning_effort tuple PLUS the config generation.
	// The generation is included so that any config change (reload, credential
	// update, etc.) invalidates the cache, preventing stale clients from being
	// reused (task #341, P1-3).
	//
	// An earlier version of this cache called buildModelsFromCfg once per
	// slot, swapping (largeCfg, smallCfg) argument order to "select" which
	// half of the pair to keep for the small slot. That swap silently
	// mismatched roles: buildModelsFromCfg(ctx, smallCfg, largeCfg, false)
	// returns (ModelBuiltFromSmallCfg, ModelBuiltFromLargeCfg) — i.e. its
	// SECOND return value (labeled "small" only by the caller's own local
	// variable name) is actually built from largeCfg. The old code then
	// picked that second value as the small-model result, so
	// resolved.small ended up holding a Model built from the LARGE
	// config's provider/model whenever large and small differed — pinned
	// onto every call's SmallModel (title generation and any other
	// small-model-driven path) via resolvedOverrides.pin. Caching the pair
	// from one call, in the caller-supplied role order, removes the
	// swap entirely.
	//
	// Use the atomic generation from Snapshot(), not a separate Generation()
	// call, to ensure consistency (task #341, P1-3).
	pairCacheKey := fmt.Sprintf("gen:%d|%s:%s:%s|%s:%s:%s",
		gen,
		largeCfg.Provider, largeCfg.Model, largeCfg.ReasoningEffort,
		smallCfg.Provider, smallCfg.Model, smallCfg.ReasoningEffort)

	// c.modelCache is nil for any *coordinator built as a struct literal
	// instead of via NewCoordinator (several existing test fixtures in this
	// package do exactly that — see e.g. newWorkerToolTestCoordinator).
	// csync.Map's methods dereference the receiver's mutex, so calling
	// Get/Set on a nil *csync.Map panics; treat a nil cache as "caching
	// disabled" rather than requiring every coordinator constructor to
	// remember to initialize it.
	var largeModel, smallModel Model
	var cacheHit bool
	if c.modelCache != nil {
		if cached, ok := c.modelCache.Get(pairCacheKey); ok {
			largeModel, smallModel, cacheHit = cached.large, cached.small, true
		}
	}
	if !cacheHit {
		largeModel, smallModel, err = c.buildModelsFromCfg(ctx, cfg, largeCfg, smallCfg, false)
		if err != nil {
			return nil, fmt.Errorf("failed to build models: %w", err)
		}
		if c.modelCache != nil {
			c.modelCache.Set(pairCacheKey, cachedModelPair{large: largeModel, small: smallModel})
		}
	}

	resolved := &resolvedOverrides{
		large: largeModel,
		small: smallModel,
	}

	// Resolve prompt prefix from provider config using the same atomic snapshot.
	largeProviderCfg, ok := cfg.Providers.Get(largeModel.ModelCfg.Provider)
	if !ok {
		return nil, fmt.Errorf("large model provider %s not configured", largeModel.ModelCfg.Provider)
	}
	if largeProviderCfg.SystemPromptPrefix != "" {
		resolved.promptPrefix = largeProviderCfg.SystemPromptPrefix
	}
	// Carry the provider config resolved from THIS SAME snapshot (cfg/gen
	// above) so callers that need provider options/credentials later in the
	// same logical operation (runInternal's 401 rebuildCall, in particular)
	// don't have to take a second, independently-timed Snapshot() call that
	// could observe a different generation than the model was built from
	// (task #341, P1-1).
	resolved.providerCfg = largeProviderCfg

	// Build system prompt if a template is available. workerSubAgentActive
	// takes the SAME pinned cfg used for largeModel/largeProviderCfg above
	// (task #341, P1-1) — it used to read c.cfg.Config() live here, which
	// meant a reload landing between the Snapshot() at the top of this
	// function and this Build call could make the system prompt's
	// WorkerAvailable flag disagree with the model/prefix this call already
	// resolved from an earlier generation.
	if c.prompt != nil {
		newSystemPrompt, err := c.prompt.Build(ctx, largeModel.ModelCfg.Provider, largeModel.ModelCfg.Model, c.cfg, c.workerSubAgentActive(cfg))
		if err != nil {
			// Leave resolved.systemPrompt empty rather than guessing: the
			// caller treats "" as "nothing to pin", so the turn falls back to
			// the agent's shared prompt exactly as it did before this returned
			// anything at all.
			slog.Error("resolveSessionModels: failed to rebuild system prompt", "err", err)
		} else {
			resolved.systemPrompt = newSystemPrompt
		}
	}

	return resolved, nil
}

// resolveSubAgentModelOverride resolves sessionID's explicit worker-slot
// override (if any) into a ready-to-pin Model, for runSubAgent's per-call
// LargeModel pin (task #466). sessionID is the PARENT session dispatching
// the sub-agent, not the freshly created child sub-agent session.
//
// Returns (nil, nil) whenever there is nothing session-specific to apply —
// including when the session never set a worker override — so callers can
// cheaply fall back to the coordinator-wide default agent's own model
// (already built from the merged system/folder config in buildAgentModels)
// instead of rebuilding one. This mirrors resolveSessionModels' large/small
// cascade (session DB override -> merged system/folder config) but is
// intentionally a SEPARATE, lighter path: worker-slot resolution is only
// needed when actually dispatching a sub-agent, not on every top-level turn,
// so it isn't folded into the hot resolveSessionModels call.
//
// reviewer has no equivalent runtime hook: unlike large/small/worker, it is
// consumed only as a `crush run --role reviewer` CLI selection (an entire
// top-level run's model choice), never read at sub-agent dispatch time —
// see internal/cmd/run.go's --role docs. A session-level ReviewerModelID is
// stored (task #466's DB/API layer) for forward compatibility but currently
// has no live runtime effect; this is a deliberate, documented scoping
// decision, not an oversight.
func (c *coordinator) resolveSubAgentModelOverride(ctx context.Context, sessionID string) (*Model, error) {
	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to load session %q: %w", sessionID, err)
	}
	if sess.WorkerModelID == "" {
		return nil, nil
	}

	cfg, _ := c.cfg.Snapshot()
	workerCfg := cfg.Models[config.SelectedModelTypeWorker]
	if workerCfg.Provider != sess.WorkerModelProvider || workerCfg.Model != sess.WorkerModelID {
		workerCfg.Think = false
		workerCfg.ReasoningEffort = ""
	}
	workerCfg.Provider = sess.WorkerModelProvider
	workerCfg.Model = sess.WorkerModelID
	if sess.WorkerModelReasoningEffort != "" {
		workerCfg.ReasoningEffort = sess.WorkerModelReasoningEffort
	}

	// buildModelsFromCfg builds a large+small PAIR; pass workerCfg for both
	// slots and keep only the first result — there is no single-model
	// variant, and building the (identical, discarded) second model is
	// cheap relative to the provider round-trip this whole path exists for.
	model, _, err := c.buildModelsFromCfg(ctx, cfg, workerCfg, workerCfg, true)
	if err != nil {
		return nil, fmt.Errorf("failed to build session worker model override: %w", err)
	}
	return &model, nil
}

// applyModelOverrides builds a resolvedOverrides snapshot from explicit override parameters.
// This is used by RunWithOverrides which receives overrides directly from the caller
// (rather than from the session DB).
//
// IMPORTANT: This method does NOT write to shared state. The old behavior of writing
// to c.currentAgent.SetModels/SetSystemPromptPrefix/SetSystemPrompt has been removed
// to prevent per-session model changes from affecting other concurrent sessions.
//
// The returned snapshot is what makes a TURN immune to the shared state moving
// underneath it — no turn reads shared state after this point.
func (c *coordinator) applyModelOverrides(ctx context.Context, large, small *ModelOverride) (*resolvedOverrides, error) {
	// Atomically capture config and generation up front (task #341, P1-1) so
	// largeCfg/smallCfg below, the buildModelsFromCfg call further down, and
	// the provider/prompt reads at the end of this function all agree on one
	// generation. This function used to read Models[Large]/Models[Small] via
	// a live c.cfg.Config() call here and take a SEPARATE Snapshot() call
	// later just for buildModelsFromCfg's provider lookups — a reload
	// landing between the two could hand back a large/small model selection
	// from one generation built against provider config from another.
	cfg, _ := c.cfg.Snapshot()
	largeCfg := cfg.Models[config.SelectedModelTypeLarge]
	smallCfg := cfg.Models[config.SelectedModelTypeSmall]

	if large != nil {
		if largeCfg.Provider != large.Provider || largeCfg.Model != large.Model {
			largeCfg.Think = false
			largeCfg.ReasoningEffort = ""
		}
		largeCfg.Provider = large.Provider
		largeCfg.Model = large.Model
		if large.ReasoningEffort != "" {
			largeCfg.ReasoningEffort = large.ReasoningEffort
		}
	}
	if small != nil {
		if smallCfg.Provider != small.Provider || smallCfg.Model != small.Model {
			smallCfg.Think = false
			smallCfg.ReasoningEffort = ""
		}
		smallCfg.Provider = small.Provider
		smallCfg.Model = small.Model
		if small.ReasoningEffort != "" {
			smallCfg.ReasoningEffort = small.ReasoningEffort
		}
	}

	// Build models directly without using the cache — model overrides are
	// transient, per-run values that don't benefit from caching (each override
	// is unique to the caller's request), and the cache key pattern requires
	// the config generation which we'd need to recompute here. The cost of
	// building the fantasy.LanguageModel client is paid once per override use,
	// which is acceptable since overrides are explicitly opt-in per-call.
	// Reuse the cfg captured at the top of this function (task #341, P1-1)
	// instead of taking a second, separately-timed Snapshot() here.
	largeModel, smallModel, err := c.buildModelsFromCfg(ctx, cfg, largeCfg, smallCfg, false)
	if err != nil {
		return nil, fmt.Errorf("failed to build override models: %w", err)
	}

	resolved := &resolvedOverrides{large: largeModel, small: smallModel}

	if largeProviderCfg, ok := cfg.Providers.Get(largeModel.ModelCfg.Provider); ok {
		resolved.promptPrefix = largeProviderCfg.SystemPromptPrefix
		resolved.providerCfg = largeProviderCfg
	}
	// workerSubAgentActive takes the SAME pinned cfg used for largeModel
	// above (task #341, P1-1) rather than re-reading c.cfg.Config() live.
	if c.prompt != nil {
		newSystemPrompt, err := c.prompt.Build(ctx, largeModel.ModelCfg.Provider, largeModel.ModelCfg.Model, c.cfg, c.workerSubAgentActive(cfg))
		if err != nil {
			slog.Error("applyModelOverrides: failed to rebuild system prompt", "err", err)
		} else {
			resolved.systemPrompt = newSystemPrompt
		}
	}
	return resolved, nil
}

// pin copies the resolved values onto a call so the turn runs on them
// regardless of what another session does to the shared agent meanwhile.
// nil receiver = no overrides were applied, so nothing to pin — the call
// keeps whatever runInternal/buildCall already put there.
func (r *resolvedOverrides) pin(call *SessionAgentCall) {
	if r == nil {
		return
	}
	large, small := r.large, r.small
	call.LargeModel = &large
	call.SmallModel = &small
	if r.promptPrefix != "" {
		prefix := r.promptPrefix
		call.SystemPromptPrefix = &prefix
	}
	if r.systemPrompt != "" {
		prompt := r.systemPrompt
		call.SystemPrompt = &prompt
	}
}

// resolveSessionSystemPrompt loads the per-session system prompt from the DB,
// building and persisting one on the fly when missing. Shared by runInternal
// and buildCall.
func (c *coordinator) resolveSessionSystemPrompt(ctx context.Context, sessionID string) string {
	sess, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return ""
	}
	if sess.SystemPrompt != "" {
		return sess.SystemPrompt
	}
	if c.prompt == nil {
		return ""
	}

	// Resolve the session's model to use for prompt building.
	// Use the large model since that's what the turn runs on.
	resolved, resolveErr := c.resolveSessionModels(ctx, sessionID)
	if resolveErr != nil {
		slog.Warn("coordinator: failed to resolve models for system prompt", "sessionID", sessionID, "err", resolveErr)
		return ""
	}

	// Reuse the system prompt resolveSessionModels already built from its
	// OWN single pinned config snapshot (task #341, P1-1), instead of
	// rebuilding it here from a second, separately-timed live cfg read
	// (c.workerSubAgentActive() with no argument, and c.cfg.Config()
	// implicitly via prompt.Build's store). A reload landing between the
	// two builds could otherwise make this second build's WorkerAvailable
	// flag disagree with resolved.large/resolved.promptPrefix, which were
	// pinned from an earlier generation.
	built := resolved.systemPrompt
	if built == "" {
		return ""
	}
	if saveErr := c.sessions.UpdateSystemPrompt(ctx, sessionID, built); saveErr != nil {
		slog.Warn("coordinator: failed to save system prompt to session", "sessionID", sessionID, "err", saveErr)
	}
	return built
}

// workerSubAgentActive reports whether a sub-agent being built right now is
// acting as a "worker": the parent run is driven by the Large ("smart") slot
// — or the active role is unknown, which for the interactive TUI/web path is
// equivalent to smart — AND a Worker model is actually configured. This is
// the single shared predicate for "the sub-agent should behave like a
// worker", used both to pick the sub-agent's model (buildAgentModels, below)
// and to pick its tool set (buildTools): a sub-agent that gets the Worker
// model but stays read-only, or vice versa, would defeat the point of the
// feature. isSubAgent must be checked by the caller first — this method
// assumes it's already true and only re-checks the role/config gate.
//
// cfg MUST be the same *config.Config the caller already pinned via
// Snapshot() for the rest of its build/resolve operation (task #341, P1-1).
// This method used to take no cfg argument and read c.cfg.Config() live
// instead — a torn read: buildAgentModels captured one generation via
// Snapshot() for the large/small/worker model lookups, then this method
// re-read Models[Worker] from whatever generation happened to be published
// at the moment it ran. A reload landing in between could hand back a
// worker slot from a DIFFERENT generation than the one buildAgentModels
// otherwise built from, up to and including a zero-value model or a
// provider lookup that no longer resolves. Threading the same *config.Config
// through closes that gap: every reader of "is worker active" within one
// resolve/build operation now agrees on exactly one generation.
//
// Mirrors the semantics documented on buildAgentModels below: falls through
// to false (today's behavior) when Worker isn't configured, or when the
// operator explicitly chose a non-large role (fast/worker/reviewer) for the
// whole run — we don't second-guess that choice by force-upgrading/
// downgrading sub-agents. Fork patch (reviewer/worker roles).
func (c *coordinator) workerSubAgentActive(cfg *config.Config) bool {
	c.activeModelRoleMu.Lock()
	activeRole := c.activeModelRole
	c.activeModelRoleMu.Unlock()

	if activeRole != "" && activeRole != config.SelectedModelTypeLarge {
		return false
	}

	workerModelCfg, ok := cfg.Models[config.SelectedModelTypeWorker]
	return ok && workerModelCfg.Model != ""
}

// TODO: when we support multiple agents we need to change this so that we pass in the agent specific model config
func (c *coordinator) buildAgentModels(ctx context.Context, isSubAgent bool) (Model, Model, error) {
	// Single atomic snapshot for every config read below (task #341/P1-3;
	// gap found by independent review — this function used to read
	// large/small/worker each via a separate c.cfg.Config() call, then take
	// a fourth, separate Snapshot() just for buildModelsFromCfg's provider
	// lookups. A reload landing between any of those reads could produce a
	// cross-generation mix, exactly what Snapshot() exists to prevent.
	cfg, _ := c.cfg.Snapshot()

	largeModelCfg, ok := cfg.Models[config.SelectedModelTypeLarge]
	if !ok {
		return Model{}, Model{}, errLargeModelNotSelected
	}
	smallModelCfg, ok := cfg.Models[config.SelectedModelTypeSmall]
	if !ok {
		return Model{}, Model{}, errSmallModelNotSelected
	}

	// Fork patch (reviewer/worker roles): when spawning a sub-agent acting as
	// a worker (see workerSubAgentActive), prefer the cheaper Worker slot for
	// the sub-agent's large-model slot. This never touches the small-model
	// slot, and falls through to today's behavior (Large for everything)
	// otherwise.
	if isSubAgent && c.workerSubAgentActive(cfg) {
		largeModelCfg = cfg.Models[config.SelectedModelTypeWorker]
	}

	return c.buildModelsFromCfg(ctx, cfg, largeModelCfg, smallModelCfg, isSubAgent)
}

// buildModelsFromCfg builds Model objects from explicit SelectedModel configs.
// The cfg parameter must be from a single atomic Snapshot() call to ensure
// consistency across all provider reads (task #341, P1-3).
func (c *coordinator) buildModelsFromCfg(ctx context.Context, cfg *config.Config, largeModelCfg, smallModelCfg config.SelectedModel, isSubAgent bool) (Model, Model, error) {
	largeProviderCfg, ok := cfg.Providers.Get(largeModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errLargeModelProviderNotConfigured
	}

	largeProvider, err := c.buildProvider(largeProviderCfg, largeModelCfg, isSubAgent)
	if err != nil {
		return Model{}, Model{}, err
	}

	smallProviderCfg, ok := cfg.Providers.Get(smallModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errSmallModelProviderNotConfigured
	}

	smallProvider, err := c.buildProvider(smallProviderCfg, smallModelCfg, true)
	if err != nil {
		return Model{}, Model{}, err
	}

	var largeCatwalkModel *catwalk.Model
	var smallCatwalkModel *catwalk.Model

	for _, m := range largeProviderCfg.Models {
		if m.ID == largeModelCfg.Model {
			largeCatwalkModel = &m
		}
	}
	for _, m := range smallProviderCfg.Models {
		if m.ID == smallModelCfg.Model {
			smallCatwalkModel = &m
		}
	}

	if largeCatwalkModel == nil {
		return Model{}, Model{}, errLargeModelNotFound
	}

	if smallCatwalkModel == nil {
		return Model{}, Model{}, errSmallModelNotFound
	}

	largeModelID := largeModelCfg.Model
	smallModelID := smallModelCfg.Model

	if largeModelCfg.Provider == openrouter.Name && isExactoSupported(largeModelID) {
		largeModelID += ":exacto"
	}

	if smallModelCfg.Provider == openrouter.Name && isExactoSupported(smallModelID) {
		smallModelID += ":exacto"
	}

	largeModel, err := largeProvider.LanguageModel(ctx, largeModelID)
	if err != nil {
		return Model{}, Model{}, err
	}
	smallModel, err := smallProvider.LanguageModel(ctx, smallModelID)
	if err != nil {
		return Model{}, Model{}, err
	}

	return Model{
			Model:      largeModel,
			CatwalkCfg: *largeCatwalkModel,
			ModelCfg:   largeModelCfg,
			FlatRate:   largeProviderCfg.FlatRate,
		}, Model{
			Model:      smallModel,
			CatwalkCfg: *smallCatwalkModel,
			ModelCfg:   smallModelCfg,
			FlatRate:   smallProviderCfg.FlatRate,
		}, nil
}

// Model returns the globally configured large model from config.
// This is used for display/status queries and does NOT reflect any per-session
// model overrides. After the per-session model isolation fix, callers that need
// the actual model for a specific session should use resolveSessionModels instead.
func (c *coordinator) Model() Model {
	// Build the default large model from config without caching (this is
	// called infrequently, mostly for status display).
	cfg, _ := c.cfg.Snapshot()
	largeCfg := cfg.Models[config.SelectedModelTypeLarge]
	smallCfg := cfg.Models[config.SelectedModelTypeSmall]

	// Create a temporary context for model building.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	largeModel, _, err := c.buildModelsFromCfg(ctx, cfg, largeCfg, smallCfg, false)
	if err != nil {
		// Return a zero-value model rather than panicking on status queries.
		slog.Error("coordinator.Model: failed to build default large model", "err", err)
		return Model{}
	}
	return largeModel
}

func (c *coordinator) UpdateModels(ctx context.Context) error {
	c.clearModelCache()

	// build the models again so we make sure we get the latest config
	large, small, err := c.buildAgentModels(ctx, false)
	if err != nil {
		return err
	}
	c.currentAgent.SetModels(large, small)

	// Update prompt prefix for the new large model provider
	if largeProviderCfg, ok := c.cfg.Config().Providers.Get(large.ModelCfg.Provider); ok {
		c.currentAgent.SetSystemPromptPrefix(largeProviderCfg.SystemPromptPrefix)
	}

	agentCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return errCoderAgentNotConfigured
	}

	tools, err := c.buildTools(ctx, agentCfg, false)
	if err != nil {
		return err
	}
	c.currentAgent.SetTools(tools)
	return nil
}

// clearModelCache empties the model cache, forcing the next resolveSessionModels
// call to rebuild models from the current config. This is called after credential
// updates (OAuth token refresh, API key re-resolution) to ensure cached models
// with stale clients are not reused (task #341, P1-3).
func (c *coordinator) clearModelCache() {
	if c.modelCache != nil {
		c.modelCache.Reset(make(map[string]cachedModelPair))
	}
}
