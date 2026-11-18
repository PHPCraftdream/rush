// Usage and cost accounting shared by runTurn, the compaction paths, and
// title generation: openrouter cost overrides and session token/cost updates.
package agent

import (
	"log/slog"
	"sync"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"

	core "github.com/PHPCraftdream/rush/internal/session/service"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
)

// unpricedModels holds the models already warned about: one warning per
// model per process.
var unpricedModels sync.Map

// modelUnpriced reports whether a step that consumed tokens is costed at zero
// only because the model carries no price (not in the catalog, no override):
// its spend never reaches a budget. Flat-rate models and steps with a
// provider-reported cost are priced by other means.
func modelUnpriced(model Model, usage fantasy.Usage, overrideCost *float64) bool {
	if model.FlatRate || overrideCost != nil {
		return false
	}
	cfg := model.CatwalkCfg
	if cfg.CostPer1MIn != 0 || cfg.CostPer1MOut != 0 || cfg.CostPer1MInCached != 0 || cfg.CostPer1MOutCached != 0 {
		return false
	}
	return usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.CacheReadTokens != 0 || usage.CacheCreationTokens != 0
}

// warnIfUnpriced logs, once per model, that the model has no price and so no
// budget applies to it.
func warnIfUnpriced(model Model, usage fantasy.Usage, overrideCost *float64) {
	if !modelUnpriced(model, usage, overrideCost) {
		return
	}
	provider, id := model.ModelCfg.Provider, model.ModelCfg.Model
	if id == "" {
		id = model.CatwalkCfg.ID
	}
	if _, seen := unpricedModels.LoadOrStore(provider+"/"+id, struct{}{}); seen {
		return
	}
	slog.Warn("Model has no price: its usage is counted as free and no budget applies to it",
		"provider", provider, "model", id)
}

func (a *sessionAgent) openrouterCost(metadata fantasy.ProviderMetadata) *float64 {
	openrouterMetadata, ok := metadata[openrouter.Name]
	if !ok {
		return nil
	}

	opts, ok := openrouterMetadata.(*openrouter.ProviderMetadata)
	if !ok {
		return nil
	}
	return &opts.Usage.Cost
}

// updateSessionUsage computes the cost delta for this step, applies the
// new token snapshot to session in-place (token fields are last-snapshot
// overwrite semantics), and returns the cost delta. The caller MUST
// persist the cost delta via sessions.IncrementCost (race-safe additive
// UPDATE) rather than relying on Save, because Save no longer writes the
// cost column.
//
// Fork patch (concurrency): upstream version was void; we now return
// the delta and rely on the caller to drive IncrementCost. See
// CHANGELOG.fork.md (Section 4.I).
//
// Fork merge note (origin/main 6ed8852b / 2e9c6505 / 74e6e378 "fix(agent):
// estimate/harden fallback usage accounting"): adopted upstream's
// updateSessionTokenCounters helper so partial-zero usage chunks no longer
// overwrite accumulated counters with zero. Rejected their `estimated bool`
// parameter (drives session.EstimatedUsage marker — a TUI widget we do not
// ship, see CHANGELOG.fork.md Section 2) and their eventTokensUsed publish
// (no consumer in our WebSocket fan-out).
func (a *sessionAgent) updateSessionUsage(model Model, session *core.Session, usage fantasy.Usage, overrideCost *float64) float64 {
	modelConfig := model.CatwalkCfg
	cost := modelConfig.CostPer1MInCached/1e6*float64(usage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(usage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(usage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(usage.OutputTokens)

	warnIfUnpriced(model, usage, overrideCost)

	// Use override cost if available (e.g., from OpenRouter).
	if overrideCost != nil {
		cost = *overrideCost
	}

	// Skip cost accumulation
	if model.FlatRate {
		cost = 0
	}

	// #1130: OwnCost mirrors the node's own ledger (cost_self) for the
	// in-memory snapshot; the persisted truth is written by the caller's
	// IncrementCost. Cap decisions read the subtree budget instead.
	session.OwnCost += cost
	provauth.UpdateSessionTokenCounters(session, usage)
	return cost
}
