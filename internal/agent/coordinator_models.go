// Per-session model resolution: override snapshots, pinning, and the
// model-pair cache. Extracted from coordinator.go — pure code move,
// bodies unchanged.

package agent

import (
	"context"
	"log/slog"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/config"
	session "github.com/PHPCraftdream/rush/internal/session/service"
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
