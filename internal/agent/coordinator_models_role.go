package agent

import (
	"context"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
)

// workerSubAgentActiveForCall is the per-call form of workerSubAgentActive
// (R1-1): the active role comes from the context's CallOptions when present,
// and only falls back to the coordinator-wide activeModelRole field for
// legacy callers whose context carries none. The fallback read keeps the
// existing mutex discipline; the per-call read is race-free by construction
// (an immutable value bound to the context before the build started).
func (c *coordinator) workerSubAgentActiveForCall(ctx context.Context, cfg *config.Config) bool {
	if creds := callCredentialsFrom(ctx); creds != nil {
		if opts := callOptionsFrom(ctx); opts != nil && opts.ModelRole != "" {
			if opts.ModelRole != config.SelectedModelTypeSmart {
				return false
			}
		}
		// An explicit tenant worker always enables worker/orchestrator
		// policy. When it is omitted, retain the established smart-run
		// orchestration policy driven by the configured worker slot; the
		// actual credentialed sub-agent model still falls back to tenant
		// smart and never uses that operator worker.
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
	return c.workerSubAgentActive(cfg)
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
	c.activeModelRoleMu.Lock()
	activeRole := c.activeModelRole
	c.activeModelRoleMu.Unlock()

	if activeRole != "" && activeRole != config.SelectedModelTypeSmart {
		return false
	}

	workerModelCfg, ok := cfg.Models[config.SelectedModelTypeWorker]
	return ok && workerModelCfg.Model != ""
}

// callModelRole returns the model role this call was started with (--role),
// from the context's CallOptions. "" means no options or no role: the run
// rides the smart slot with no role-derived resolution at all.
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
	source := hbSourceSlotStart
	switch {
	case role == config.SelectedModelTypeFast && sess.FastModelID != "":
		sel = config.SelectedModel{Provider: sess.FastModelProvider, Model: sess.FastModelID, ReasoningEffort: sess.FastModelReasoningEffort}
		source = hbSourceSessionOverride
	case role == config.SelectedModelTypeWorker && sess.WorkerModelID != "":
		sel = config.SelectedModel{Provider: sess.WorkerModelProvider, Model: sess.WorkerModelID, ReasoningEffort: sess.WorkerModelReasoningEffort}
		source = hbSourceSessionOverride
	case role == config.SelectedModelTypeReviewer && sess.ReviewerModelID != "":
		sel = config.SelectedModel{Provider: sess.ReviewerModelProvider, Model: sess.ReviewerModelID, ReasoningEffort: sess.ReviewerModelReasoningEffort}
		source = hbSourceSessionOverride
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
	return &ModelOverride{Provider: sel.Provider, Model: sel.Model, ReasoningEffort: sel.ReasoningEffort}, source, true
}

func (c *coordinator) refreshDrainModel(ctx context.Context, role config.SelectedModelType) (Model, config.ProviderConfig, bool) {
	cfg, _ := c.cfg.Snapshot()
	slot, ok := cfg.Models[role]
	if !ok || slot.Model == "" {
		return Model{}, config.ProviderConfig{}, false
	}
	providerCfg, ok := cfg.Providers.Get(slot.Provider)
	if !ok {
		return Model{}, config.ProviderConfig{}, false
	}
	rebuilt, _, err := c.buildModelsFromCfg(ctx, cfg, slot, slot, true)
	if err != nil {
		slog.Warn("Drain: keeping the model built at the delegation's start; the live slot rebuild failed", "err", err)
		return Model{}, config.ProviderConfig{}, false
	}
	return rebuilt, providerCfg, true
}

// refreshSubAgentSlotModel re-resolves the slot-built sub-agent model from
// the live config snapshot at dispatch time. It always rebuilds effective
// settings even when provider and model identifiers are unchanged.
func (c *coordinator) refreshSubAgentSlotModel(ctx context.Context, cfg *config.Config, role config.SelectedModelType, model Model) (Model, bool) {
	slot, ok := cfg.Models[role]
	if !ok || slot.Model == "" {
		return model, false
	}
	rebuilt, _, err := c.buildModelsFromCfg(ctx, cfg, slot, slot, true)
	if err != nil {
		slog.Warn("runSubAgent: keeping the model built at the parent turn's start; the live slot rebuild failed", "err", err)
		return model, false
	}
	return rebuilt, true
}
