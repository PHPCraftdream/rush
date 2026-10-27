// Per-session model resolution: override snapshots, pinning, and the
// model-pair cache. Extracted from coordinator.go — pure code move,
// bodies unchanged.

package agent

import (
	"context"
	"fmt"
	"log/slog"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
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
	smart Model
	fast  Model
	// smartSource/fastSource are the heartbeat source labels decided where
	// the slots were resolved; runInternal attaches them to the turn ctx.
	smartSource  string
	fastSource   string
	promptPrefix string
	systemPrompt string
	// orchestrator is the worker/orchestrator mode this snapshot's systemPrompt
	// AND tools were built in — the very bool workerSubAgentActiveForCall
	// returned for this same cfg. sessionPromptForCall
	// (coordinator_session_prompt.go) compares it against the rule 7 marker in
	// the session's stored prompt, so a session inherited from before a worker
	// was configured gets its prompt rebuilt instead of running a prompt that
	// contradicts its tool set (A24).
	orchestrator bool
	// providerCfg is the smart model's provider config, resolved from the
	// SAME Snapshot()/Config() call that built `smart` above. Callers that
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
	// credentials is non-nil when this snapshot was resolved from a
	// per-call CredentialSet (RunWithCredentials) instead of session
	// DB/config state. runInternal's 401 rebuild re-resolves through the
	// SAME credentials so a rejected tenant key can never retry the turn
	// on the operator's configured provider.
	credentials *CredentialSet
	// tools is the coder toolset built from the SAME pinned config snapshot
	// and THIS call's context (R3-1): buildTools reads the per-call
	// CallOptions (DisableSubAgents, ModelRole) from the context, so the
	// slice is scoped to exactly one call. pin copies it onto the
	// SessionAgentCall so the turn consumes its own toolset at start and at
	// every PrepareStep instead of re-reading the shared currentAgent
	// tools, which a concurrent call's UpdateModels/buildTools used to
	// overwrite mid-turn. nil (coder agent unconfigured, build failed) =
	// pin nothing; the call falls back to the shared toolset exactly like a
	// legacy caller.
	tools []fantasy.AgentTool
	// Explicit ExecuteRun model-slot writes, applied only after the session
	// agent owns the inter-process lock.
	persistSmartModel *session.ModelSlotUpdate
	persistFastModel  *session.ModelSlotUpdate
}

// resolveSessionModels builds the full per-call snapshot for a turn —
// models, prompt, AND the per-call coder toolset (R3-1).
func (c *coordinator) resolveSessionModels(ctx context.Context, sessionID string) (*resolvedOverrides, error) {
	return c.resolveSessionModelsInternal(ctx, sessionID, true)
}

// resolveSessionModels builds an immutable snapshot of model configuration for a session.
// It reads from the session DB if overrides are present, otherwise falls back to
// the global config defaults. This method NEVER writes to shared state (c.currentAgent),
// ensuring that per-session model choices don't affect other concurrent sessions.
//
// The returned snapshot includes both smart and fast models, the provider's system
// prompt prefix, and the built system prompt (if a prompt template is available).
//
// Results are cached per unique structured key containing the config generation
// and each smart/fast provider, model, and reasoning-effort field.
// The config generation is included so that any config change (reload, credential update,
// etc.) invalidates the cache, preventing stale clients from being reused (task #341, P1-3).
//
// All config reads use a single atomic Snapshot() call to prevent reading config fields
// from different generations (task #341, P1-3).
func (c *coordinator) resolveSessionModelsInternal(ctx context.Context, sessionID string, wantTools bool) (*resolvedOverrides, error) {
	// Load session preferences only when policy permits them.
	var sess session.Session
	var err error
	if WebModelPolicy(ctx) {
		sess, err = c.sessions.Get(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("failed to load session %q: %w", sessionID, err)
		}
	}

	// Atomically capture config and generation in a single snapshot to prevent
	// torn reads across reloads (task #341, P1-3).
	cfg, gen := c.cfg.Snapshot()

	// Start with the global config defaults.
	smartCfg := cfg.Models[config.SelectedModelTypeSmart]
	fastCfg := cfg.Models[config.SelectedModelTypeFast]

	var smartOverride, fastOverride *ModelOverride
	if sess.SmartModelID != "" {
		smartOverride = &ModelOverride{Provider: sess.SmartModelProvider, Model: sess.SmartModelID, ReasoningEffort: sess.SmartModelReasoningEffort}
	}
	if sess.FastModelID != "" {
		fastOverride = &ModelOverride{Provider: sess.FastModelProvider, Model: sess.FastModelID, ReasoningEffort: sess.FastModelReasoningEffort}
	}

	// D2 (docs/plans/2026-10-15-role-models-hot-reload.md §2.2): a
	// role-derived value is not an explicit choice. When the call carries a
	// non-smart ModelRole and the session has no explicit smart override,
	// the smart slot resolves through the SHARED roleSlotOverride resolver
	// (session pin tier first, then the role's config slot) — re-read on
	// EVERY turn from this pinned snapshot, so a hot-reloaded role model
	// reaches the next request. roleReasoningEffortFrom carries the explicit
	// --effort of a non-smart role run onto the resolved slot (plan R6).
	roleSource := ""
	if smartOverride == nil {
		role := callModelRole(ctx)
		if role == config.SelectedModelTypeSmart || role == "" {
			if effort := roleReasoningEffortFrom(ctx); effort != "" {
				smartCfg.ReasoningEffort = effort
			}
		}
		if role != config.SelectedModelTypeSmart && role != "" {
			if ovr, src, ok := roleSlotOverride(cfg, sess, role, roleReasoningEffortFrom(ctx)); ok {
				smartOverride = ovr
				roleSource = src
			}
		}
	}

	// Where each slot's model came from is recorded for heartbeat
	// attribution (design pt. 3); the labels ride the returned snapshot.

	// Merge overrides into the config copies.
	if smartOverride != nil {
		if smartCfg.Provider != smartOverride.Provider || smartCfg.Model != smartOverride.Model {
			smartCfg.Think = false
			smartCfg.ReasoningEffort = ""
		}
		smartCfg.Provider = smartOverride.Provider
		smartCfg.Model = smartOverride.Model
		if smartOverride.ReasoningEffort != "" {
			smartCfg.ReasoningEffort = smartOverride.ReasoningEffort
		}
	}
	if fastOverride != nil {
		if fastCfg.Provider != fastOverride.Provider || fastCfg.Model != fastOverride.Model {
			fastCfg.Think = false
			fastCfg.ReasoningEffort = ""
		}
		fastCfg.Provider = fastOverride.Provider
		fastCfg.Model = fastOverride.Model
		if fastOverride.ReasoningEffort != "" {
			fastCfg.ReasoningEffort = fastOverride.ReasoningEffort
		}
	}

	// Build (or reuse from cache) both models TOGETHER in a single
	// buildModelsFromCfg call, keyed by the combined smart+fast
	// provider+model+reasoning_effort tuple PLUS the config generation.
	// The generation is included so that any config change (reload, credential
	// update, etc.) invalidates the cache, preventing stale clients from being
	// reused (task #341, P1-3).
	//
	// An earlier version of this cache called buildModelsFromCfg once per
	// slot, swapping (largeCfg, smallCfg) argument order to "select" which
	// half of the pair to keep for the fast slot. That swap silently
	// mismatched roles: buildModelsFromCfg(ctx, smallCfg, largeCfg, false)
	// returns (ModelBuiltFromSmallCfg, ModelBuiltFromLargeCfg) — i.e. its
	// SECOND return value (labeled "fast" only by the caller's own local
	// variable name) is actually built from largeCfg. The old code then
	// picked that second value as the fast-model result, so
	// resolved.fast ended up holding a Model built from the SMART
	// config's provider/model whenever the smart and fast configs differed — pinned
	// onto every call's FastModel (title generation and any other
	// fast-model-driven path) via resolvedOverrides.pin. Caching the pair
	// from one call, in the caller-supplied role order, removes the
	// swap entirely.
	//
	// Use the atomic generation from Snapshot(), not a separate Generation()
	// call, to ensure consistency (task #341, P1-3).
	pairCacheKey := modelPairCacheKey{
		generation:     gen,
		smartProvider:  smartCfg.Provider,
		smartModel:     smartCfg.Model,
		smartReasoning: smartCfg.ReasoningEffort,
		fastProvider:   fastCfg.Provider,
		fastModel:      fastCfg.Model,
		fastReasoning:  fastCfg.ReasoningEffort,
	}

	// c.modelCache is nil for any *coordinator built as a struct literal
	// instead of via NewCoordinator (several existing test fixtures in this
	// package do exactly that — see e.g. newWorkerToolTestCoordinator).
	// Treat a nil cache as "caching disabled" rather than requiring every
	// coordinator constructor to remember to initialize it.
	var smartModel, fastModel Model
	cached, cacheHit, cacheEpoch := c.getCachedModelPair(pairCacheKey)
	if cacheHit {
		smartModel, fastModel = cached.smart, cached.fast
	}
	if !cacheHit {
		smartModel, fastModel, err = c.buildCachedModelPair(ctx, cfg, smartCfg, fastCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to build models: %w", err)
		}
		c.setCachedModelPairIfCurrent(pairCacheKey, cachedModelPair{smart: smartModel, fast: fastModel}, cacheEpoch)
	}

	resolved := &resolvedOverrides{
		smart: smartModel,
		fast:  fastModel,
	}
	resolved.smartSource = hbOverrideSource(smartOverride != nil)
	resolved.fastSource = hbOverrideSource(fastOverride != nil)
	if roleSource != "" {
		resolved.smartSource = roleSource
	}
	resolved.persistSmartModel, resolved.persistFastModel = sessionModelPersistenceFrom(ctx)

	// Resolve prompt prefix from provider config using the same atomic snapshot.
	smartProviderCfg, ok := cfg.Providers.Get(smartModel.ModelCfg.Provider)
	if !ok {
		return nil, fmt.Errorf("smart model provider %s not configured", smartModel.ModelCfg.Provider)
	}
	if smartProviderCfg.SystemPromptPrefix != "" {
		resolved.promptPrefix = smartProviderCfg.SystemPromptPrefix
	}
	// Carry the provider config resolved from THIS SAME snapshot (cfg/gen
	// above) so callers that need provider options/credentials later in the
	// same logical operation (runInternal's 401 rebuildCall, in particular)
	// don't have to take a second, independently-timed Snapshot() call that
	// could observe a different generation than the model was built from
	// (task #341, P1-1).
	resolved.providerCfg = smartProviderCfg
	if err := c.rejectScopedCallOnCLIProvider(ctx, "smart", smartProviderCfg); err != nil {
		return nil, err
	}
	// The worker role runs on the same rule: a scoped call's sub-agent
	// spawns resolve through the Worker slot, so a CLI-typed worker
	// provider is equally unusable there.
	if workerModelCfg, ok := cfg.Models[config.SelectedModelTypeWorker]; ok && workerModelCfg.Model != "" {
		if workerProviderCfg, ok := cfg.Providers.Get(workerModelCfg.Provider); ok {
			if err := c.rejectScopedCallOnCLIProvider(ctx, "worker", workerProviderCfg); err != nil {
				return nil, err
			}
		}
	}

	// Build system prompt if a template is available. workerSubAgentActive
	// takes the SAME pinned cfg used for smartModel/smartProviderCfg above
	// (task #341, P1-1) — it used to read c.cfg.Config() live here, which
	// meant a reload landing between the Snapshot() at the top of this
	// function and this Build call could make the system prompt's
	// WorkerAvailable flag disagree with the model/prefix this call already
	// resolved from an earlier generation.
	if c.prompt != nil {
		newSystemPrompt, err := c.prompt.Build(ctx, smartModel.ModelCfg.Provider, smartModel.ModelCfg.Model, c.cfg, cfg, c.workerSubAgentActiveForCall(ctx, cfg))
		if err != nil {
			// Leave resolved.systemPrompt empty rather than guessing: the
			// caller treats "" as "nothing to pin", so the turn falls back to
			// the agent's shared prompt exactly as it did before this returned
			// anything at all.
			slog.Error("resolveSessionModels: failed to rebuild system prompt", "err", err)
		} else {
			resolved.systemPrompt = newSystemPrompt
			resolved.orchestrator = c.workerSubAgentActiveForCall(ctx, cfg)
		}
	}

	// R3-1: when the caller runs a turn (not a prompt-only resolve), build
	// THIS call's coder toolset from the SAME pinned cfg and with THIS
	// call's context, so the per-call filters (CallOptions.DisableSubAgents,
	// ModelRole) decide the slice and the result is pinned to the call
	// instead of published onto the shared currentAgent. The old publisher
	// was ExecuteRun's per-run UpdateModels, whose SetTools overwrote the
	// ONE shared toolset every in-flight turn re-read at every step.
	if wantTools {
		if opts := callOptionsFrom(ctx); opts != nil {
			EnsureCallBuildScope(opts)
		}
		tools, err := c.pinCallTools(ctx, cfg)
		if err != nil {
			return nil, err
		}
		resolved.tools = tools
	}

	return resolved, nil
}

// resolveSubAgentModelOverride resolves sessionID's explicit worker-slot
// override (if any) into a ready-to-pin Model, for runSubAgent's per-call
// SmartModel pin (task #466). sessionID is the PARENT session dispatching
// the sub-agent, not the freshly created child sub-agent session.
//
// Returns (nil, nil) whenever there is nothing session-specific to apply —
// including when the session never set a worker override — so callers can
// cheaply fall back to the coordinator-wide default agent's own model
// (already built from the merged system/folder config in buildAgentModels)
// instead of rebuilding one. This mirrors resolveSessionModels' smart/fast
// cascade (session DB override -> merged system/folder config) but is
// intentionally a SEPARATE, lighter path: worker-slot resolution is only
// needed when actually dispatching a sub-agent, not on every top-level turn,
// so it isn't folded into the hot resolveSessionModels call.
//
// reviewer has no equivalent SUB-AGENT DISPATCH hook: unlike worker, it is
// never read when the "agent" tool spawns a sub-agent, because reviewer is
// never a delegation target. A top-level `rush run --role reviewer`
// follows the configured reviewer slot (internal/cmd/run_role_model.go).
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

	// buildModelsFromCfg builds a smart+fast PAIR; pass workerCfg for both
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
func (c *coordinator) applyModelOverrides(ctx context.Context, sessionID string, smart, fast *ModelOverride) (*resolvedOverrides, error) {
	// Atomically capture config and generation up front (task #341, P1-1) so
	// largeCfg/smallCfg below, the buildModelsFromCfg call further down, and
	// the provider/prompt reads at the end of this function all agree on one
	// generation. This function used to read Models[Smart]/Models[Fast] via
	// a live c.cfg.Config() call here and take a SEPARATE Snapshot() call
	// later just for buildModelsFromCfg's provider lookups — a reload
	// landing between the two could hand back a smart/fast model selection
	// from one generation built against provider config from another.
	cfg, _ := c.cfg.Snapshot()
	// The role-slot cascade needs the session's worker/reviewer/fast pins;
	// a failed load resolves no pins (the caller decides whether that is
	// fatal — the explicit overrides below still apply).
	sess := session.Session{}
	if sessionID != "" && WebModelPolicy(ctx) {
		if loaded, getErr := c.sessions.Get(ctx, sessionID); getErr == nil {
			sess = loaded
		}
	}
	smartCfg := cfg.Models[config.SelectedModelTypeSmart]
	fastCfg := cfg.Models[config.SelectedModelTypeFast]
	// Where each slot came from is recorded for heartbeat attribution;
	// the labels ride the returned snapshot.

	if smart != nil {
		if smartCfg.Provider != smart.Provider || smartCfg.Model != smart.Model {
			smartCfg.Think = false
			smartCfg.ReasoningEffort = ""
		}
		smartCfg.Provider = smart.Provider
		smartCfg.Model = smart.Model
		if smart.ReasoningEffort != "" {
			smartCfg.ReasoningEffort = smart.ReasoningEffort
		}
	}

	// D2 (plan §2.2) via the SHARED roleSlotOverride resolver: an omitted
	// smart override on a non-smart role run takes the role's session pin,
	// else the role's config slot, from the same pinned snapshot — the pin
	// tier applies here too, so a fast override riding along never buries
	// a worker pin (plan P1-2). roleReasoningEffortFrom carries the explicit
	// --effort of a non-smart role run onto the resolved slot (plan R6).
	roleSource := ""
	if smart == nil {
		role := callModelRole(ctx)
		if role == config.SelectedModelTypeSmart || role == "" {
			if effort := roleReasoningEffortFrom(ctx); effort != "" {
				smartCfg.ReasoningEffort = effort
			}
		}
		if role != config.SelectedModelTypeSmart && role != "" {
			if ovr, src, ok := roleSlotOverride(cfg, sess, role, roleReasoningEffortFrom(ctx)); ok {
				if smartCfg.Provider != ovr.Provider || smartCfg.Model != ovr.Model {
					smartCfg.Think = false
					smartCfg.ReasoningEffort = ""
				}
				smartCfg.Provider = ovr.Provider
				smartCfg.Model = ovr.Model
				if ovr.ReasoningEffort != "" {
					smartCfg.ReasoningEffort = ovr.ReasoningEffort
				}
				roleSource = src
			}
		}
	}
	if fast != nil {
		if fastCfg.Provider != fast.Provider || fastCfg.Model != fast.Model {
			fastCfg.Think = false
			fastCfg.ReasoningEffort = ""
		}
		fastCfg.Provider = fast.Provider
		fastCfg.Model = fast.Model
		if fast.ReasoningEffort != "" {
			fastCfg.ReasoningEffort = fast.ReasoningEffort
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
	smartModel, fastModel, err := c.buildModelsFromCfg(ctx, cfg, smartCfg, fastCfg, false)
	if err != nil {
		return nil, fmt.Errorf("failed to build override models: %w", err)
	}

	resolved := &resolvedOverrides{smart: smartModel, fast: fastModel}
	resolved.smartSource = hbCallSource(smart != nil)
	resolved.fastSource = hbCallSource(fast != nil)
	if roleSource != "" {
		resolved.smartSource = roleSource
	}
	resolved.persistSmartModel, resolved.persistFastModel = sessionModelPersistenceFrom(ctx)

	if smartProviderCfg, ok := cfg.Providers.Get(smartModel.ModelCfg.Provider); ok {
		resolved.promptPrefix = smartProviderCfg.SystemPromptPrefix
		resolved.providerCfg = smartProviderCfg
		if err := c.rejectScopedCallOnCLIProvider(ctx, "smart", smartProviderCfg); err != nil {
			return nil, err
		}
	}
	// Same worker-role rule as resolveSessionModelsInternal: a scoped
	// call's sub-agent spawns resolve through the Worker slot.
	if workerModelCfg, ok := cfg.Models[config.SelectedModelTypeWorker]; ok && workerModelCfg.Model != "" {
		if workerProviderCfg, ok := cfg.Providers.Get(workerModelCfg.Provider); ok {
			if err := c.rejectScopedCallOnCLIProvider(ctx, "worker", workerProviderCfg); err != nil {
				return nil, err
			}
		}
	}
	// workerSubAgentActive takes the SAME pinned cfg used for smartModel
	// above (task #341, P1-1) rather than re-reading c.cfg.Config() live.
	if c.prompt != nil {
		newSystemPrompt, err := c.prompt.Build(ctx, smartModel.ModelCfg.Provider, smartModel.ModelCfg.Model, c.cfg, cfg, c.workerSubAgentActiveForCall(ctx, cfg))
		if err != nil {
			slog.Error("applyModelOverrides: failed to rebuild system prompt", "err", err)
		} else {
			resolved.systemPrompt = newSystemPrompt
			resolved.orchestrator = c.workerSubAgentActiveForCall(ctx, cfg)
		}
	}
	// R3-1: pin the per-call coder toolset exactly like
	// resolveSessionModels does — built from the same pinned cfg and this
	// call's context, so RunWithOverrides' per-call policy decides the
	// slice instead of the shared agent state.
	tools, err := c.pinCallTools(ctx, cfg)
	if err != nil {
		return nil, err
	}
	resolved.tools = tools
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
	smart, fast := r.smart, r.fast
	call.SmartModel = &smart
	call.FastModel = &fast
	if r.promptPrefix != "" {
		prefix := r.promptPrefix
		call.SystemPromptPrefix = &prefix
	}
	if r.systemPrompt != "" {
		prompt := r.systemPrompt
		call.SystemPrompt = &prompt
	}
	if r.tools != nil {
		call.Tools = r.tools
	}
	call.PersistSmartModel = r.persistSmartModel
	call.PersistFastModel = r.persistFastModel
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
	// Use the smart model since that's what the turn runs on.
	// wantTools=false: this path only needs the prompt, and the turn that
	// consumes it builds (and pins) its own toolset — building one here
	// would double the per-turn tool build for no effect.
	resolved, resolveErr := c.resolveSessionModelsInternal(ctx, sessionID, false)
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
	// flag disagree with resolved.smart/resolved.promptPrefix, which were
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
