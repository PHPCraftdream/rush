package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"

	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/callbeat"
	"github.com/PHPCraftdream/rush/internal/config"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
)

type modelsHost interface {
	cfg() *config.ConfigStore
	sessions() coresession.Service
	prompt() *prompt.Prompt
	currentAgent() SessionAgent
	activeModelRoleMu() *sync.Mutex
	activeModelRole() config.SelectedModelType
	modelCacheMu() *sync.Mutex
	bumpModelCacheEpoch()
	modelCache() modelkit.ModelPairCache
	getCachedModelPair(key modelkit.ModelPairCacheKey) (modelkit.CachedModelPair, bool, uint64)
	buildCachedModelPair(ctx context.Context, cfg *config.Config, smartCfg, fastCfg config.SelectedModel) (Model, Model, error)
	setCachedModelPairIfCurrent(key modelkit.ModelPairCacheKey, pair modelkit.CachedModelPair, epoch uint64)
	buildCredentialModel(ctx context.Context, cfg *config.Config, creds *CredentialSet, choice ModelChoice) (Model, config.ProviderConfig, error)
	buildProvider(cfg *config.Config, providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool) (fantasy.Provider, error)
	buildTools(ctx context.Context, cfg *config.Config, agent config.Agent, isSubAgent bool) ([]fantasy.AgentTool, error)
	rejectScopedCallOnCLIProvider(ctx context.Context, role string, providerCfg config.ProviderConfig) error
}

type (
	modelsComponent       struct{ host modelsHost }
	coordinatorModelsHost struct{ coordinator *coordinator }
)

func newModelsComponent(c *coordinator) *modelsComponent {
	return &modelsComponent{host: coordinatorModelsHost{coordinator: c}}
}

func (h coordinatorModelsHost) cfg() *config.ConfigStore      { return h.coordinator.cfg }
func (h coordinatorModelsHost) sessions() coresession.Service { return h.coordinator.sessions }
func (h coordinatorModelsHost) prompt() *prompt.Prompt        { return h.coordinator.prompt }
func (h coordinatorModelsHost) currentAgent() SessionAgent    { return h.coordinator.currentAgent }
func (h coordinatorModelsHost) activeModelRoleMu() *sync.Mutex {
	return &h.coordinator.activeModelRoleMu
}

func (h coordinatorModelsHost) activeModelRole() config.SelectedModelType {
	return h.coordinator.activeModelRole
}

func (h coordinatorModelsHost) modelCacheMu() *sync.Mutex { return &h.coordinator.modelCacheMu }

func (h coordinatorModelsHost) bumpModelCacheEpoch() { h.coordinator.modelCacheEpoch++ }

func (h coordinatorModelsHost) modelCache() modelkit.ModelPairCache { return h.coordinator.modelCache }

func (h coordinatorModelsHost) getCachedModelPair(key modelkit.ModelPairCacheKey) (modelkit.CachedModelPair, bool, uint64) {
	return h.coordinator.getCachedModelPair(key)
}

func (h coordinatorModelsHost) buildCachedModelPair(ctx context.Context, cfg *config.Config, smartCfg, fastCfg config.SelectedModel) (Model, Model, error) {
	return h.coordinator.buildCachedModelPair(ctx, cfg, smartCfg, fastCfg)
}

func (h coordinatorModelsHost) setCachedModelPairIfCurrent(key modelkit.ModelPairCacheKey, pair modelkit.CachedModelPair, epoch uint64) {
	h.coordinator.setCachedModelPairIfCurrent(key, pair, epoch)
}

func (h coordinatorModelsHost) buildCredentialModel(ctx context.Context, cfg *config.Config, creds *CredentialSet, choice ModelChoice) (Model, config.ProviderConfig, error) {
	return h.coordinator.buildCredentialModel(ctx, cfg, creds, choice)
}

func (h coordinatorModelsHost) buildProvider(cfg *config.Config, providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool) (fantasy.Provider, error) {
	return h.coordinator.buildProvider(cfg, providerCfg, model, isSubAgent)
}

func (h coordinatorModelsHost) buildTools(ctx context.Context, cfg *config.Config, agent config.Agent, isSubAgent bool) ([]fantasy.AgentTool, error) {
	return h.coordinator.buildTools(ctx, cfg, agent, isSubAgent)
}

func (h coordinatorModelsHost) rejectScopedCallOnCLIProvider(ctx context.Context, role string, providerCfg config.ProviderConfig) error {
	return h.coordinator.rejectScopedCallOnCLIProvider(ctx, role, providerCfg)
}

func (p *modelsComponent) resolveSessionModels(ctx context.Context, sessionID string) (*resolvedOverrides, error) {
	return p.resolveSessionModelsInternal(ctx, sessionID, true)
}

func (p *modelsComponent) resolveSessionModelsInternal(ctx context.Context, sessionID string, wantTools bool) (*resolvedOverrides, error) {
	var sess coresession.Session
	var err error
	if WebModelPolicy(ctx) && callModelRole(ctx) != config.SelectedModelTypeReviewer {
		sess, err = p.host.sessions().Get(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("failed to load session %q: %w", sessionID, err)
		}
	}
	cfg, gen := p.host.cfg().Snapshot()
	smartCfg := cfg.Models[config.SelectedModelTypeSmart]
	fastCfg := cfg.Models[config.SelectedModelTypeFast]
	var smartOverride, fastOverride *ModelOverride
	if sess.SmartModelID != "" {
		smartOverride = &ModelOverride{Provider: sess.SmartModelProvider, Model: sess.SmartModelID, ReasoningEffort: sess.SmartModelReasoningEffort}
	}
	if sess.FastModelID != "" {
		fastOverride = &ModelOverride{Provider: sess.FastModelProvider, Model: sess.FastModelID, ReasoningEffort: sess.FastModelReasoningEffort}
	}
	roleSource := ""
	customContract := (*coresession.RoleContract)(nil)
	customRequested := false
	if opts := callOptionsFrom(ctx); opts != nil && opts.RoleContract != nil && opts.RoleContract.Name != "" {
		customContract = opts.RoleContract
		customRequested = true
		if err := customContract.Validate(); err != nil {
			return nil, fmt.Errorf("invalid custom role contract: %w", err)
		}
		selected, source, roleErr := modelkit.RoleContractPromptSource(cfg, customContract)
		if roleErr != nil {
			return nil, roleErr
		}
		smartCfg = selected
		smartOverride = nil
		roleSource = source
	} else if smartOverride == nil {
		role := callModelRole(ctx)
		if role == config.SelectedModelTypeSmart || role == "" {
			if effort := roleReasoningEffortFrom(ctx); effort != "" {
				smartCfg.ReasoningEffort = effort
			}
		}
		if role != config.SelectedModelTypeSmart && role != "" {
			if ovr, src, ok := roleSlotOverride(cfg, sess, role, roleReasoningEffortFrom(ctx)); ok {
				smartOverride = ovr
				smartCfg.ServiceTier = ""
				roleSource = src
			}
		}
	}

	// Where each slot's model came from is recorded for heartbeat
	// attribution (design pt. 3); the labels ride the returned snapshot.

	// Merge overrides into the config copies.
	if opts := callOptionsFrom(ctx); opts != nil && opts.RoleContract != nil && opts.RoleContract.Name != "" && !customRequested {
		return nil, fmt.Errorf("custom role selection is missing its contract snapshot")
	}
	if customRequested {
		model := modelkit.CloneSelectedModel(smartCfg)
		selectedModel, _, buildErr := p.buildModelsFromCfg(ctx, cfg, model, model, false)
		if buildErr != nil {
			return nil, fmt.Errorf("failed to build custom role model: %w", buildErr)
		}
		return p.finishCustomRoleResolve(ctx, cfg, selectedModel, smartCfg, roleSource, wantTools)
	}
	if smartOverride != nil {
		if smartCfg.Provider != smartOverride.Provider || smartCfg.Model != smartOverride.Model {
			smartCfg.Think = false
			smartCfg.ReasoningEffort = ""
			smartCfg.ServiceTier = ""
		}
		smartCfg.Provider = smartOverride.Provider
		smartCfg.Model = smartOverride.Model
		if smartOverride.ReasoningEffort != "" {
			smartCfg.ReasoningEffort = smartOverride.ReasoningEffort
		}
		smartCfg.ServiceTier = cmp.Or(smartOverride.ServiceTier, smartCfg.ServiceTier)
	}
	if fastOverride != nil {
		if fastCfg.Provider != fastOverride.Provider || fastCfg.Model != fastOverride.Model {
			fastCfg.Think = false
			fastCfg.ReasoningEffort = ""
			fastCfg.ServiceTier = ""
		}
		fastCfg.Provider = fastOverride.Provider
		fastCfg.Model = fastOverride.Model
		if fastOverride.ReasoningEffort != "" {
			fastCfg.ReasoningEffort = fastOverride.ReasoningEffort
		}
		fastCfg.ServiceTier = cmp.Or(fastOverride.ServiceTier, fastCfg.ServiceTier)
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
	pairCacheKey := modelkit.NewModelPairCacheKey(gen, smartCfg, fastCfg)

	// p.host.modelCache() is nil for any *coordinator built as a struct literal
	// instead of via NewCoordinator (several existing test fixtures in this
	// package do exactly that — see e.g. newWorkerToolTestCoordinator).
	// Treat a nil cache as "caching disabled" rather than requiring every
	// coordinator constructor to remember to initialize it.
	var smartModel, fastModel Model
	cached, cacheHit, cacheEpoch := p.host.getCachedModelPair(pairCacheKey)
	if cacheHit {
		smartModel, fastModel = cached.Smart, cached.Fast
	}
	if !cacheHit {
		smartModel, fastModel, err = p.host.buildCachedModelPair(ctx, cfg, smartCfg, fastCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to build models: %w", err)
		}
		p.host.setCachedModelPairIfCurrent(pairCacheKey, modelkit.CachedModelPair{Smart: smartModel, Fast: fastModel}, cacheEpoch)
	}

	resolved := &resolvedOverrides{
		smart: smartModel,
		fast:  fastModel,
	}
	if customContract != nil {
		resolved.smartSource = callbeat.HbSourceSessionOverride
		resolved.fastSource = callbeat.HbSourceSlotLive
	} else {
		resolved.smartSource = callbeat.HbOverrideSource(smartOverride != nil)
		resolved.fastSource = callbeat.HbOverrideSource(fastOverride != nil)
		if roleSource != "" {
			resolved.smartSource = roleSource
		}
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
	if err := p.host.rejectScopedCallOnCLIProvider(ctx, "smart", smartProviderCfg); err != nil {
		return nil, err
	}
	// The worker role runs on the same rule: a scoped call's sub-agent
	// spawns resolve through the Worker slot, so a CLI-typed worker
	// provider is equally unusable there.
	if workerModelCfg, ok := cfg.Models[config.SelectedModelTypeWorker]; ok && workerModelCfg.Model != "" {
		if workerProviderCfg, ok := cfg.Providers.Get(workerModelCfg.Provider); ok {
			if err := p.host.rejectScopedCallOnCLIProvider(ctx, "worker", workerProviderCfg); err != nil {
				return nil, err
			}
		}
	}

	// Build system prompt if a template is available. workerSubAgentActive
	// takes the SAME pinned cfg used for smartModel/smartProviderCfg above
	// (task #341, P1-1) — it used to read p.host.cfg().Config() live here, which
	// meant a reload landing between the Snapshot() at the top of this
	// function and this Build call could make the system prompt's
	// WorkerAvailable flag disagree with the model/prefix this call already
	// resolved from an earlier generation.
	if p.host.prompt() != nil {
		newSystemPrompt, err := p.host.prompt().Build(ctx, smartModel.ModelCfg.Provider, smartModel.ModelCfg.Model, p.host.cfg(), cfg, p.workerSubAgentActiveForCall(ctx, cfg))
		if err != nil {
			// Leave resolved.systemPrompt empty rather than guessing: the
			// caller treats "" as "nothing to pin", so the turn falls back to
			// the agent's shared prompt exactly as it did before this returned
			// anything at all.
			slog.Error("resolveSessionModels: failed to rebuild system prompt", "err", err)
		} else {
			resolved.systemPrompt = newSystemPrompt
			resolved.orchestrator = p.workerSubAgentActiveForCall(ctx, cfg)
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
		tools, err := p.pinCallTools(ctx, cfg)
		if err != nil {
			return nil, err
		}
		resolved.tools = tools
	}

	return resolved, nil
}

func (p *modelsComponent) finishCustomRoleResolve(ctx context.Context, cfg *config.Config, smartModel Model, selected config.SelectedModel, source string, wantTools bool) (*resolvedOverrides, error) {
	return p.finishCustomRoleResolveSnapshot(ctx, cfg, smartModel, selected, source, wantTools)
}

func (p *modelsComponent) resolveSubAgentModelOverride(ctx context.Context, sessionID string) (*Model, error) {
	sess, err := p.host.sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to load session %q: %w", sessionID, err)
	}
	if sess.WorkerModelID == "" {
		return nil, nil
	}
	cfg, _ := p.host.cfg().Snapshot()
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
	model, _, err := p.buildModelsFromCfg(ctx, cfg, workerCfg, workerCfg, true)
	if err != nil {
		return nil, fmt.Errorf("failed to build session worker model override: %w", err)
	}
	return &model, nil
}

func (p *modelsComponent) applyModelOverrides(ctx context.Context, sessionID string, smart, fast *ModelOverride) (*resolvedOverrides, error) {
	// Atomically capture config and generation up front (task #341, P1-1) so
	// largeCfg/smallCfg below, the buildModelsFromCfg call further down, and
	// the provider/prompt reads at the end of this function all agree on one
	// generation. This function used to read Models[Smart]/Models[Fast] via
	// a live p.host.cfg().Config() call here and take a SEPARATE Snapshot() call
	// later just for buildModelsFromCfg's provider lookups — a reload
	// landing between the two could hand back a smart/fast model selection
	// from one generation built against provider config from another.
	cfg, _ := p.host.cfg().Snapshot()
	// The role-slot cascade needs the session's worker/reviewer/fast pins;
	// a failed load resolves no pins (the caller decides whether that is
	// fatal — the explicit overrides below still apply).
	sess := coresession.Session{}
	if sessionID != "" && WebModelPolicy(ctx) {
		if loaded, getErr := p.host.sessions().Get(ctx, sessionID); getErr == nil {
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
				smartCfg.ServiceTier = ovr.ServiceTier
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
			fastCfg.ServiceTier = ""
		}
		fastCfg.Provider = fast.Provider
		fastCfg.Model = fast.Model
		if fast.ReasoningEffort != "" {
			fastCfg.ReasoningEffort = fast.ReasoningEffort
		}
		fastCfg.ServiceTier = cmp.Or(fast.ServiceTier, fastCfg.ServiceTier)
	}

	// Build models directly without using the cache — model overrides are
	// transient, per-run values that don't benefit from caching (each override
	// is unique to the caller's request), and the cache key pattern requires
	// the config generation which we'd need to recompute here. The cost of
	// building the fantasy.LanguageModel client is paid once per override use,
	// which is acceptable since overrides are explicitly opt-in per-call.
	// Reuse the cfg captured at the top of this function (task #341, P1-1)
	// instead of taking a second, separately-timed Snapshot() here.
	smartModel, fastModel, err := p.buildModelsFromCfg(ctx, cfg, smartCfg, fastCfg, false)
	if err != nil {
		return nil, fmt.Errorf("failed to build override models: %w", err)
	}

	resolved := &resolvedOverrides{smart: smartModel, fast: fastModel}
	resolved.smartSource = callbeat.HbCallSource(smart != nil)
	resolved.fastSource = callbeat.HbCallSource(fast != nil)
	if roleSource != "" {
		resolved.smartSource = roleSource
	}
	resolved.persistSmartModel, resolved.persistFastModel = sessionModelPersistenceFrom(ctx)

	if smartProviderCfg, ok := cfg.Providers.Get(smartModel.ModelCfg.Provider); ok {
		resolved.promptPrefix = smartProviderCfg.SystemPromptPrefix
		resolved.providerCfg = smartProviderCfg
		if err := p.host.rejectScopedCallOnCLIProvider(ctx, "smart", smartProviderCfg); err != nil {
			return nil, err
		}
	}
	// Same worker-role rule as resolveSessionModelsInternal: a scoped
	// call's sub-agent spawns resolve through the Worker slot.
	if workerModelCfg, ok := cfg.Models[config.SelectedModelTypeWorker]; ok && workerModelCfg.Model != "" {
		if workerProviderCfg, ok := cfg.Providers.Get(workerModelCfg.Provider); ok {
			if err := p.host.rejectScopedCallOnCLIProvider(ctx, "worker", workerProviderCfg); err != nil {
				return nil, err
			}
		}
	}
	// workerSubAgentActive takes the SAME pinned cfg used for smartModel
	// above (task #341, P1-1) rather than re-reading p.host.cfg().Config() live.
	if p.host.prompt() != nil {
		newSystemPrompt, err := p.host.prompt().Build(ctx, smartModel.ModelCfg.Provider, smartModel.ModelCfg.Model, p.host.cfg(), cfg, p.workerSubAgentActiveForCall(ctx, cfg))
		if err != nil {
			slog.Error("applyModelOverrides: failed to rebuild system prompt", "err", err)
		} else {
			resolved.systemPrompt = newSystemPrompt
			resolved.orchestrator = p.workerSubAgentActiveForCall(ctx, cfg)
		}
	}
	// R3-1: pin the per-call coder toolset exactly like
	// resolveSessionModels does — built from the same pinned cfg and this
	// call's context, so RunWithOverrides' per-call policy decides the
	// slice instead of the shared agent state.
	tools, err := p.pinCallTools(ctx, cfg)
	if err != nil {
		return nil, err
	}
	resolved.tools = tools
	return resolved, nil
}

func (p *modelsComponent) buildAgentModels(ctx context.Context, isSubAgent bool) (Model, Model, error) {
	// Single atomic snapshot for every config read below (task #341/P1-3;
	// gap found by independent review — this function used to read
	// smart/fast/worker each via a separate p.host.cfg().Config() call, then take
	// a fourth, separate Snapshot() just for buildModelsFromCfg's provider
	// lookups. A reload landing between any of those reads could produce a
	// cross-generation mix, exactly what Snapshot() exists to prevent.
	cfg, _ := p.host.cfg().Snapshot()
	return p.buildAgentModelsFromCfg(ctx, cfg, isSubAgent)
}

func (p *modelsComponent) buildAgentModelsFromCfg(ctx context.Context, cfg *config.Config, isSubAgent bool) (Model, Model, error) {
	if opts := callOptionsFrom(ctx); opts != nil && opts.RoleContract != nil && opts.RoleContract.Name != "" {
		return p.buildRoleContractModels(ctx, cfg, opts.RoleContract)
	}
	if creds := callCredentialsFrom(ctx); creds != nil {
		choice := creds.Models[RoleSmart]
		if isSubAgent {
			// A worker choice is the only tenant worker slot. When it is
			// omitted, dispatch deliberately falls back to tenant smart.
			if workerChoice, ok := creds.Models[RoleWorker]; ok {
				choice = workerChoice
			}
		}
		smart, _, err := p.host.buildCredentialModel(ctx, cfg, creds, choice)
		if err != nil {
			return Model{}, Model{}, err
		}
		fast := smart
		if fastChoice, ok := creds.Models[RoleFast]; ok {
			fast, _, err = p.host.buildCredentialModel(ctx, cfg, creds, fastChoice)
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
	if isSubAgent && p.workerSubAgentActiveForCall(ctx, cfg) {
		smartModelCfg = cfg.Models[config.SelectedModelTypeWorker]
	}

	return p.buildModelsFromCfg(ctx, cfg, smartModelCfg, fastModelCfg, isSubAgent)
}

func (p *modelsComponent) buildRoleContractModels(ctx context.Context, cfg *config.Config, contract *coresession.RoleContract) (Model, Model, error) {
	selected, _, err := modelkit.CurrentOrSnapshotRoleModel(cfg, contract)
	if err != nil {
		return Model{}, Model{}, err
	}
	return p.buildModelsFromCfg(ctx, cfg, selected, selected, true)
}

func (p *modelsComponent) buildModelsFromCfg(ctx context.Context, cfg *config.Config, smartModelCfg, fastModelCfg config.SelectedModel, isSubAgent bool) (Model, Model, error) {
	smartProviderCfg, ok := cfg.Providers.Get(smartModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errSmartModelProviderNotConfigured
	}

	smartProvider, err := p.host.buildProvider(cfg, smartProviderCfg, smartModelCfg, isSubAgent)
	if err != nil {
		return Model{}, Model{}, err
	}

	fastProviderCfg, ok := cfg.Providers.Get(fastModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errFastModelProviderNotConfigured
	}

	fastProvider, err := p.host.buildProvider(cfg, fastProviderCfg, fastModelCfg, true)
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

func (p *modelsComponent) Model() Model {
	// Build the default smart model from config without caching (this is
	// called infrequently, mostly for status display).
	cfg, _ := p.host.cfg().Snapshot()
	smartCfg := cfg.Models[config.SelectedModelTypeSmart]
	fastCfg := cfg.Models[config.SelectedModelTypeFast]

	// Create a temporary context for model building.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	smartModel, _, err := p.buildModelsFromCfg(ctx, cfg, smartCfg, fastCfg, false)
	if err != nil {
		// Return a zero-value model rather than panicking on status queries.
		slog.Error("coordinator.Model: failed to build default smart model", "err", err)
		return Model{}
	}
	return smartModel
}

func (p *modelsComponent) UpdateModels(ctx context.Context) error {
	ctx = withoutCallOptions(ctx)
	p.clearModelCache()

	// build the models again so we make sure we get the latest config
	cfg, _ := p.host.cfg().Snapshot()
	smart, fast, err := p.buildAgentModelsFromCfg(ctx, cfg, false)
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
	tools, err := p.host.buildTools(ctx, cfg, agentCfg, false)
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
	p.host.currentAgent().SetModels(smart, fast)
	if smartProviderCfg, ok := cfg.Providers.Get(smart.ModelCfg.Provider); ok {
		p.host.currentAgent().SetSystemPromptPrefix(smartProviderCfg.SystemPromptPrefix)
	}
	p.host.currentAgent().SetTools(tools)
	return nil
}

func (p *modelsComponent) clearModelCache() {
	p.host.modelCacheMu().Lock()
	defer p.host.modelCacheMu().Unlock()

	p.host.bumpModelCacheEpoch()
	if p.host.modelCache() == nil {
		return
	}
	if cache, ok := p.host.modelCache().(*modelkit.BoundedModelPairCache); ok {
		cache.Clear()
		return
	}
	if cache, ok := p.host.modelCache().(interface {
		Reset(map[string]modelkit.CachedModelPair)
	}); ok {
		cache.Reset(make(map[string]modelkit.CachedModelPair))
	}
}

func (p *modelsComponent) pinCallTools(ctx context.Context, cfg *config.Config) ([]fantasy.AgentTool, error) {
	required := scopedCallToolsRequired(ctx)
	fail := func(err error) ([]fantasy.AgentTool, error) {
		// R12-H2a round 2: a failure AFTER buildTools has already pinned
		// construction owners into the call's build scope (the empty-
		// toolset refusal, the ready-gate drain error) must not leak
		// them. Only the CALL's own scope is closed: callBuildScopeFrom
		// returns nil for bare contexts, so the coordinator's shared
		// scope is never touched here.
		if scope := callBuildScopeFrom(ctx); scope != nil {
			scope.Close()
		}
		slog.Warn("Coordinator: failed to build per-call tools", "err", err, "distinct_toolset_required", required)
		if required {
			// Wrap both: callers match the cause (e.g. ErrDiskProviderNotDurable).
			return nil, fmt.Errorf("%w: %w", ErrScopedCallToolsUnavailable, err)
		}
		return nil, nil
	}

	agentCfg, ok := cfg.Agents[config.AgentCoder]
	if !ok {
		return fail(errCoderAgentNotConfigured)
	}
	buildGate := &readyGate{}
	ctx = withReadyGate(ctx, buildGate)
	tools, err := p.host.buildTools(ctx, cfg, agentCfg, false)
	if err != nil {
		_ = buildGate.Wait()
		return fail(err)
	}
	if pinCallToolsReadyGateSeam != nil {
		pinCallToolsReadyGateSeam(buildGate)
	}
	if required && len(tools) == 0 && !roleToolsConstrained(ctx) {
		_ = buildGate.Wait()
		return fail(errors.New("per-call toolset is empty"))
	}
	// The toolset embeds a freshly built sub-agent (buildTools -> agentTool
	// -> buildAgent), whose prompt/tool builds run asynchronously. Drain the
	// local build group before the turn uses this per-call toolset.
	if waitErr := buildGate.Wait(); waitErr != nil {
		return fail(waitErr)
	}
	return tools, nil
}

func (p *modelsComponent) finishCustomRoleResolveSnapshot(ctx context.Context, cfg *config.Config, smartModel Model, selected config.SelectedModel, source string, wantTools bool) (*resolvedOverrides, error) {
	fastCfg := cfg.Models[config.SelectedModelTypeFast]
	if fastCfg.Provider == "" || fastCfg.Model == "" {
		fastCfg = selected
	}
	fastModel, _, err := p.buildModelsFromCfg(ctx, cfg, fastCfg, fastCfg, true)
	if err != nil {
		return nil, fmt.Errorf("failed to build companion fast model: %w", err)
	}
	providerCfg, ok := cfg.Providers.Get(selected.Provider)
	if !ok {
		return nil, fmt.Errorf("custom role provider %s not configured", selected.Provider)
	}
	if err := p.host.rejectScopedCallOnCLIProvider(ctx, "smart", providerCfg); err != nil {
		return nil, err
	}
	resolved := &resolvedOverrides{
		smart:        smartModel,
		fast:         fastModel,
		smartSource:  source,
		fastSource:   callbeat.HbSourceSlotLive,
		providerCfg:  providerCfg,
		promptPrefix: providerCfg.SystemPromptPrefix,
	}
	resolved.persistSmartModel, resolved.persistFastModel = sessionModelPersistenceFrom(ctx)
	if p.host.prompt() != nil {
		prompt, promptErr := p.host.prompt().Build(ctx, smartModel.ModelCfg.Provider, smartModel.ModelCfg.Model, p.host.cfg(), cfg, false)
		if promptErr != nil {
			slog.Error("resolveSessionModels: failed to build custom-role system prompt", "err", promptErr)
		} else {
			resolved.systemPrompt = prompt
		}
	}
	resolved.orchestrator = false
	if wantTools {
		if opts := callOptionsFrom(ctx); opts != nil {
			EnsureCallBuildScope(opts)
		}
		resolved.tools, err = p.pinCallTools(ctx, cfg)
		if err != nil {
			return nil, err
		}
	}
	return resolved, nil
}

func (p *modelsComponent) workerSubAgentActiveForCall(ctx context.Context, cfg *config.Config) bool {
	if opts := callOptionsFrom(ctx); opts != nil && opts.RoleContract != nil && opts.RoleContract.Name != "" && !RequestedModelRole(opts.RoleContract.Name).Builtin() {
		return false
	}
	if remaining, err := MaxAgentDepthFrom(ctx); err != nil || remaining == 0 {
		return false
	}
	if creds := callCredentialsFrom(ctx); creds != nil {
		if opts := callOptionsFrom(ctx); opts != nil && opts.ModelRole != "" && opts.ModelRole != config.SelectedModelTypeSmart {
			return false
		}
		if _, covered := creds.Models[RoleWorker]; covered {
			return true
		}
		workerModelCfg, ok := cfg.Models[config.SelectedModelTypeWorker]
		return ok && workerModelCfg.Model != ""
	}
	if opts := callOptionsFrom(ctx); opts != nil && opts.ModelRole != "" {
		if opts.ModelRole != config.SelectedModelTypeSmart {
			return false
		}
		workerModelCfg, ok := cfg.Models[config.SelectedModelTypeWorker]
		return ok && workerModelCfg.Model != ""
	}
	return p.workerSubAgentActive(cfg)
}

func (p *modelsComponent) workerSubAgentActive(cfg *config.Config) bool {
	p.host.activeModelRoleMu().Lock()
	activeRole := p.host.activeModelRole()
	p.host.activeModelRoleMu().Unlock()

	if activeRole != "" && activeRole != config.SelectedModelTypeSmart {
		return false
	}

	workerModelCfg, ok := cfg.Models[config.SelectedModelTypeWorker]
	return ok && workerModelCfg.Model != ""
}

func (p *modelsComponent) refreshDrainModel(ctx context.Context, role config.SelectedModelType) (Model, config.ProviderConfig, bool) {
	cfg, _ := p.host.cfg().Snapshot()
	slot, ok := cfg.Models[role]
	if !ok || slot.Model == "" {
		return Model{}, config.ProviderConfig{}, false
	}
	providerCfg, ok := cfg.Providers.Get(slot.Provider)
	if !ok {
		return Model{}, config.ProviderConfig{}, false
	}
	rebuilt, _, err := p.buildModelsFromCfg(ctx, cfg, slot, slot, true)
	if err != nil {
		slog.Warn("Drain: keeping the model built at the delegation's start; the live slot rebuild failed", "err", err)
		return Model{}, config.ProviderConfig{}, false
	}
	return rebuilt, providerCfg, true
}

func (p *modelsComponent) refreshSubAgentSlotModel(ctx context.Context, cfg *config.Config, role config.SelectedModelType, model Model) (Model, bool) {
	slot, ok := cfg.Models[role]
	if !ok || slot.Model == "" {
		return model, false
	}
	rebuilt, _, err := p.buildModelsFromCfg(ctx, cfg, slot, slot, true)
	if err != nil {
		slog.Warn("runSubAgent: keeping the model built at the parent turn's start; the live slot rebuild failed", "err", err)
		return model, false
	}
	return rebuilt, true
}
