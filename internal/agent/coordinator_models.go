package agent

import (
	"context"
	"errors"
	"log/slog"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchadapter/delegation"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchflow"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/callbeat"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

func (c *coordinator) buildCachedModelPair(ctx context.Context, cfg *config.Config, smartCfg, fastCfg config.SelectedModel) (Model, Model, error) {
	if c.modelPairBuilder != nil {
		return c.modelPairBuilder(ctx, cfg, smartCfg, fastCfg)
	}
	return c.buildModelsFromCfg(ctx, cfg, smartCfg, fastCfg, false)
}

func (c *coordinator) getCachedModelPair(key modelkit.ModelPairCacheKey) (modelkit.CachedModelPair, bool, uint64) {
	c.modelCacheMu.Lock()
	defer c.modelCacheMu.Unlock()

	epoch := c.modelCacheEpoch
	if c.modelCache == nil {
		return modelkit.CachedModelPair{}, false, epoch
	}
	if cache, ok := c.modelCache.(*modelkit.BoundedModelPairCache); ok {
		pair, found := cache.Get(key)
		return pair, found, epoch
	}
	if cache, ok := c.modelCache.(interface {
		Get(string) (modelkit.CachedModelPair, bool)
	}); ok {
		pair, found := cache.Get(modelkit.LegacyModelPairCacheKey(key))
		return pair, found, epoch
	}
	return modelkit.CachedModelPair{}, false, epoch
}

func (c *coordinator) setCachedModelPairIfCurrent(key modelkit.ModelPairCacheKey, pair modelkit.CachedModelPair, epoch uint64) {
	c.modelCacheMu.Lock()
	defer c.modelCacheMu.Unlock()

	if c.modelCache == nil || c.modelCacheEpoch != epoch {
		return
	}
	if cache, ok := c.modelCache.(*modelkit.BoundedModelPairCache); ok {
		cache.Set(key, pair)
		return
	}
	if cache, ok := c.modelCache.(interface {
		Set(string, modelkit.CachedModelPair)
	}); ok {
		cache.Set(modelkit.LegacyModelPairCacheKey(key), pair)
	}
}

// Per-session model resolution: override snapshots, pinning, and the
// model-pair cache. Extracted from coordinator.go — pure code move,
// bodies unchanged.

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
	return newModelsComponent(c).resolveSessionModels(ctx, sessionID)
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
	return newModelsComponent(c).resolveSessionModelsInternal(ctx, sessionID, wantTools)
}

func (c *coordinator) finishCustomRoleResolve(ctx context.Context, cfg *config.Config, smartModel Model, selected config.SelectedModel, source string, wantTools bool) (*resolvedOverrides, error) {
	return newModelsComponent(c).finishCustomRoleResolve(ctx, cfg, smartModel, selected, source, wantTools)
}

func (c *coordinator) resolveSubAgentModelOverride(ctx context.Context, sessionID string) (*Model, error) {
	return newModelsComponent(c).resolveSubAgentModelOverride(ctx, sessionID)
}

func (c *coordinator) applyModelOverrides(ctx context.Context, sessionID string, smart, fast *ModelOverride) (*resolvedOverrides, error) {
	return newModelsComponent(c).applyModelOverrides(ctx, sessionID, smart, fast)
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

// TODO: when we support multiple agents we need to change this so that we pass in the agent specific model config
func (c *coordinator) buildAgentModels(ctx context.Context, isSubAgent bool) (Model, Model, error) {
	return newModelsComponent(c).buildAgentModels(ctx, isSubAgent)
}

// buildAgentModelsFromCfg is buildAgentModels' body, taking the config
// snapshot as a parameter instead of capturing its own (task #576/P1-3).
// buildAgent (coordinator_tools.go) pins ONE *config.Config for its entire
// build -- models, prompt, and tools -- and passes it here so the models it
// builds always agree with the prompt/toolset built from the same call. A
// caller with nothing to pin (buildAgentModels above) takes its own fresh
// Snapshot() and delegates here, so behavior for those callers is unchanged.
func (c *coordinator) buildAgentModelsFromCfg(ctx context.Context, cfg *config.Config, isSubAgent bool) (Model, Model, error) {
	return newModelsComponent(c).buildAgentModelsFromCfg(ctx, cfg, isSubAgent)
}

func (c *coordinator) buildRoleContractModels(ctx context.Context, cfg *config.Config, contract *session.RoleContract) (Model, Model, error) {
	return newModelsComponent(c).buildRoleContractModels(ctx, cfg, contract)
}

// buildModelsFromCfg builds Model objects from explicit SelectedModel configs.
// The cfg parameter must be from a single atomic Snapshot() call to ensure
// consistency across all provider reads (task #341, P1-3).
func (c *coordinator) buildModelsFromCfg(ctx context.Context, cfg *config.Config, smartModelCfg, fastModelCfg config.SelectedModel, isSubAgent bool) (Model, Model, error) {
	return newModelsComponent(c).buildModelsFromCfg(ctx, cfg, smartModelCfg, fastModelCfg, isSubAgent)
}

// Model returns the globally configured smart model from config.
// This is used for display/status queries and does NOT reflect any per-session
// model overrides. After the per-session model isolation fix, callers that need
// the actual model for a specific session should use resolveSessionModels instead.
func (c *coordinator) Model() Model { return newModelsComponent(c).Model() }

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
	return newModelsComponent(c).UpdateModels(ctx)
}

// clearModelCache empties the model cache, forcing the next resolveSessionModels
// call to rebuild models from the current config. This is called after credential
// updates (OAuth token refresh, API key re-resolution) to ensure cached models
// with stale clients are not reused (task #341, P1-3).
func (c *coordinator) clearModelCache() { newModelsComponent(c).clearModelCache() }

// ErrScopedCallToolsUnavailable is returned by resolveSessionModelsInternal/
// applyModelOverrides/resolveCredentialsModels/RunSessionAgentCall when
// pinCallTools fails to build a distinct toolset for a call whose
// CallOptions carries FolderScope, DiskProvider, DisableSubAgents, or a
// tool-shaping ModelRole (R5-1, P0; widened R6-3, P1). Every one of those
// options exists specifically to change the shared toolset's composition —
// FolderScope/DiskProvider strip the legacy view/write/edit/glob/grep/ls
// tools, MCP tools, and (usually) sub-agent tools down to a
// restricted/redirected set; DisableSubAgents strips the delegation tools
// (agent, agentic_fetch); a non-empty ModelRole decides worker-tool
// layering/orchestrator-tool stripping via workerSubAgentActiveForCall. A
// build failure for any of them means the CALLER-REQUESTED composition
// could not be built, and the shared toolset is never guaranteed to match
// it (it is built from the coordinator-wide activeModelRole/UpdateModels
// state, not this call's policy). Since an SDK run auto-approves every tool
// call, nothing downstream would catch the mismatched toolset before the
// model could use it — so the call must fail here instead of silently
// falling back, mirroring RunSessionAgentCall's existing "refuse the row"
// pattern (coordinator_interrupt.go, T12) for the durable-restart path.
var ErrScopedCallToolsUnavailable = errors.New(
	"agent: failed to build this call's tool-shaping toolset (folder-scope/disk-provider/disable-sub-agents/model-role); refusing to fall back to the shared toolset")

// scopedCallToolsRequired reports whether ctx carries per-call options that
// make a failed pinCallTools build unsafe to silently fall back from (R5-1;
// widened R6-3): FolderScope and DiskProvider mean the caller specifically
// asked for a restricted/redirected toolset; DisableSubAgents and a
// non-empty ModelRole ALSO decide this call's tool composition (see
// applyCallDisableSubAgents and workerSubAgentActiveForCall/
// buildToolsAgentConfigForCall) — a build failure for either must fail the
// call closed instead of widening/narrowing it to whatever the shared
// toolset happens to be. A legacy call (no CallOptions, or one with none of
// these fields set) keeps today's fallback behavior. Mirrors the trigger
// predicate RunSessionAgentCall uses for the durable-replay rebind
// (coordinator_interrupt.go) — kept as a single shared definition via
// scopedCallOptionsRequireDistinctTools so the two can never drift apart
// again.
func scopedCallToolsRequired(ctx context.Context) bool {
	remaining, err := MaxAgentDepthFrom(ctx)
	return err != nil || remaining == 0 || scopedCallOptionsRequireDistinctTools(callOptionsFrom(ctx))
}

// scopedCallOptionsRequireDistinctTools is scopedCallToolsRequired's
// struct-based form, usable before a CallOptions has been attached to a
// context (RunSessionAgentCall inspects the durable call's CallOptions
// directly, ahead of calling WithCallOptions).
func scopedCallOptionsRequireDistinctTools(opts *CallOptions) bool {
	return opts != nil && (opts.FolderScope != nil || tools.HasDiskProvider(opts.DiskProvider) ||
		opts.DisableSubAgents || opts.ModelRole != "" || (opts.AgentDepthSet && opts.MaxAgentDepth == 0) ||
		opts.RoleContract != nil)
}

// pinCallToolsReadyGateSeam is a test-only hook: fires immediately after the
// buildTools call below, before the local gate's Wait drain, allowing a test
// to inject a failing task on this call's gate deterministically without a race.
var pinCallToolsReadyGateSeam func(*readyGate)

// pinCallTools builds the per-call coder toolset (R3-1) from the caller's
// already-pinned cfg snapshot and THIS call's context, then drains any
// agent-build tasks registered on the ready gate. buildTools reads the
// per-call CallOptions (DisableSubAgents, ModelRole) from ctx, so the
// returned slice is scoped to exactly one call; the caller pins it onto the
// SessionAgentCall via resolvedOverrides.pin instead of publishing it to the
// shared agent.
//
// Return contract (R6-3, P1): a (nil, nil) result means "no per-call
// toolset was required — fall back to the shared toolset", the ONLY
// legitimate reason being ctx carries no CallOptions that
// scopedCallOptionsRequireDistinctTools would flag. Every other failure —
// the coder agent unconfigured, buildTools itself erroring, or a
// registered agent build failing on the ready gate — returns a non-nil
// error INSTEAD of a bare nil slice whenever ctx's CallOptions requires a
// distinct toolset (FolderScope, DiskProvider, DisableSubAgents, or a
// tool-shaping ModelRole): a nil slice was ambiguous between "no override
// requested" and "override failed", and R5-1's original version of this
// function pushed that disambiguation onto every caller individually
// (three call sites had to remember to call scopedCallToolsRequired(ctx)
// themselves) — R6-3 found that RunSessionAgentCall's durable-replay call
// site had NOT been widened to do so for DisableSubAgents/ModelRole. Moving
// the check in here means every caller gets the same answer from one place:
// err != nil always means "refuse the call", nil error always means "safe
// to use whatever slice (possibly nil) came back".
//
// For a genuinely legacy call (ctx carries no CallOptions, or one with none
// of those fields set), the same build/ready-gate failures still return
// (nil, nil) — log-only — preserving the pre-R6-3 "fall back to the shared
// toolset" behavior for callers that never asked for a distinct one.
func (c *coordinator) pinCallTools(ctx context.Context, cfg *config.Config) ([]fantasy.AgentTool, error) {
	return newModelsComponent(c).pinCallTools(ctx, cfg)
}

// withoutCallOptions returns ctx with any per-call CallOptions removed, so
// downstream builds cannot consume a caller's per-call policy (R3-1).
// UpdateModels is a GLOBAL refresh (config reloads, credential refreshes)
// and must publish only global state even when the caller's context — e.g.
// runWithUnauthorizedRetry's 401-rebuild ctx — carries CallOptions.
func withoutCallOptions(ctx context.Context) context.Context {
	// The mirrored prompt-side scope flag must go with the CallOptions:
	// a stale true here would render the global refresh's sub-builds
	// (an embedded agent tool's prompt, for instance) from a caller's
	// per-call scope, exactly what this function exists to prevent.
	return prompt.WithFolderScoped(
		context.WithValue(batchflow.GlobalQuestionContext{
			Context: ctx,
			// persistedAgentDepthContextKey is agent-owned; the batchflow
			// mask covers only the runtime/batch question keys.
			Foreign: func(key any) bool {
				return delegation.IsPersistedAgentDepthKey(key)
			},
		}, callOptionsContextKey{}, (*CallOptions)(nil)),
		false)
}

// workerSubAgentActiveForCall combines the per-call role, credentials and depth gates.
func (c *coordinator) workerSubAgentActiveForCall(ctx context.Context, cfg *config.Config) bool {
	return newModelsComponent(c).workerSubAgentActiveForCall(ctx, cfg)
}

// workerSubAgentActive reports whether a sub-agent being built right now is
// acting as a "worker": the parent run is driven by the Smart slot
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
// Snapshot() for the smart/fast/worker model lookups, then this method
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
// operator explicitly chose a non-smart role (fast/worker/reviewer) for the
// whole run — we don't second-guess that choice by force-upgrading/
// downgrading sub-agents. Fork patch (reviewer/worker roles).
func (c *coordinator) workerSubAgentActive(cfg *config.Config) bool {
	return newModelsComponent(c).workerSubAgentActive(cfg)
}

// callModelRole returns the model role this call was started with (--role),
// from the context's CallOptions. "" means no role override.
func callModelRole(ctx context.Context) config.SelectedModelType {
	opts := callOptionsFrom(ctx)
	if opts == nil {
		return ""
	}
	return opts.ModelRole
}

// roleSlotConfigModel returns the role's config slot (fast/worker/reviewer)
// from cfg. "" and smart never resolve through a role slot, and a missing
// slot key or an empty Model is "not configured" — the caller decides
// whether that is an error or a fall-through.
func roleSlotConfigModel(cfg *config.Config, role config.SelectedModelType) (config.SelectedModel, bool) {
	if role == "" || role == config.SelectedModelTypeSmart {
		return config.SelectedModel{}, false
	}
	sel, ok := cfg.Models[role]
	if !ok || sel.Model == "" {
		return config.SelectedModel{}, false
	}
	return sel, true
}

// roleSlotOverride is THE role-derived smart override resolver, shared by
// resolveSessionModelsInternal and applyModelOverrides (plan D2): the
// role's session pin wins, else the role's live config slot. roleEffort
// (the explicit --effort of a non-smart role run, plan R6) is applied to
// the resolved slot for THIS call and is never persisted — the role model
// itself is not persisted either. ""/smart roles, and a role with neither
// tier configured, return ok=false. The string result is the heartbeat
// source label for the resolved slot.
func roleSlotOverride(cfg *config.Config, sess session.Session, role config.SelectedModelType, roleEffort string) (*ModelOverride, string, bool) {
	if role == config.SelectedModelTypeSmart || role == "" {
		return nil, "", false
	}
	var sel config.SelectedModel
	source := callbeat.HbSourceSlotStart
	switch {
	case role == config.SelectedModelTypeFast && sess.FastModelID != "":
		sel = config.SelectedModel{Provider: sess.FastModelProvider, Model: sess.FastModelID, ReasoningEffort: sess.FastModelReasoningEffort}
		source = callbeat.HbSourceSessionOverride
	case role == config.SelectedModelTypeWorker && sess.WorkerModelID != "":
		sel = config.SelectedModel{Provider: sess.WorkerModelProvider, Model: sess.WorkerModelID, ReasoningEffort: sess.WorkerModelReasoningEffort}
		source = callbeat.HbSourceSessionOverride
	case role == config.SelectedModelTypeReviewer && sess.ReviewerModelID != "":
		sel = config.SelectedModel{Provider: sess.ReviewerModelProvider, Model: sess.ReviewerModelID, ReasoningEffort: sess.ReviewerModelReasoningEffort}
		source = callbeat.HbSourceSessionOverride
	default:
		slot, ok := roleSlotConfigModel(cfg, role)
		if !ok {
			return nil, "", false
		}
		sel = slot
	}
	if roleEffort != "" {
		sel.ReasoningEffort = roleEffort
	}
	return &ModelOverride{Provider: sel.Provider, Model: sel.Model, ReasoningEffort: sel.ReasoningEffort, ServiceTier: sel.ServiceTier}, source, true
}

func (c *coordinator) refreshDrainModel(ctx context.Context, role config.SelectedModelType) (Model, config.ProviderConfig, bool) {
	return newModelsComponent(c).refreshDrainModel(ctx, role)
}

// refreshSubAgentSlotModel re-resolves the slot-built sub-agent model from
// the live config snapshot at dispatch time. It always rebuilds effective
// settings even when provider and model identifiers are unchanged.
func (c *coordinator) refreshSubAgentSlotModel(ctx context.Context, cfg *config.Config, role config.SelectedModelType, model Model) (Model, bool) {
	return newModelsComponent(c).refreshSubAgentSlotModel(ctx, cfg, role, model)
}
