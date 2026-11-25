// Usage and cost accounting shared by runTurn, the compaction paths, and
// title generation: openrouter cost overrides and session token/cost updates.
package usage

import (
	"context"
	"log/slog"
	"sync"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/promptcache"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// unpricedModels holds the models already warned about: one warning per
// model per process.
var UnpricedModels sync.Map

// modelUnpriced reports whether a step that consumed tokens is costed at zero
// only because the model carries no price (not in the catalog, no override):
// its spend never reaches a budget. Flat-rate models and steps with a
// provider-reported cost are priced by other means.
func ModelUnpriced(model agentcall.Model, usage fantasy.Usage, overrideCost *float64) bool {
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
func WarnIfUnpriced(model agentcall.Model, usage fantasy.Usage, overrideCost *float64) {
	if !ModelUnpriced(model, usage, overrideCost) {
		return
	}
	provider, id := model.ModelCfg.Provider, model.ModelCfg.Model
	if id == "" {
		id = model.CatwalkCfg.ID
	}
	if _, seen := UnpricedModels.LoadOrStore(provider+"/"+id, struct{}{}); seen {
		return
	}
	slog.Warn("Model has no price: its usage is counted as free and no budget applies to it",
		"provider", provider, "model", id)
}

func OpenrouterCost(metadata fantasy.ProviderMetadata) *float64 {
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
func UpdateSessionUsage(model agentcall.Model, session *coresession.Session, usage fantasy.Usage, overrideCost *float64) float64 {
	modelConfig := model.CatwalkCfg
	cost := modelConfig.CostPer1MInCached/1e6*float64(usage.CacheCreationTokens) +
		modelConfig.CostPer1MOutCached/1e6*float64(usage.CacheReadTokens) +
		modelConfig.CostPer1MIn/1e6*float64(usage.InputTokens) +
		modelConfig.CostPer1MOut/1e6*float64(usage.OutputTokens)

	WarnIfUnpriced(model, usage, overrideCost)

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

// providerCacheSupport classifies whether a provider actually reports prompt
// caching, so a stored zero can be told apart from "we were never told".
//
// A nonzero cache counter this turn always proves Native, regardless of
// provider — a real observation trumps any list. Otherwise, classification
// falls back to cacheProfiles' knownCacheReporter facet (cache_profile.go),
// which enumerates every provider confirmed to emit cache counters at all:
// local-cli (claude/codex/gemini/qwen — see cliprovider/usage.go for the
// captured wire samples), openai-compatible (zai), openrouter/vercel/google
// (their own language_model_hooks.go/google.go; InputTokens on all three is
// corrected by normalizeProviderUsage before it reaches here), anthropic,
// bedrock. A provider NOT in that list — including any new provider type
// added without updating cacheProfiles — is classified CacheSupportNone: an
// honest "n/a" beats a fabricated 0%.
//
// The type-name list cannot carry openai-compat: the same fantasy provider
// serves cache-silent local endpoints too. Those cases are covered by
// providerID (the configured rush.json provider ID, e.g. "zai") checked
// against promptcache.IsCacheReporterProviderID (runtime/policy/promptcache).
func ProviderCacheSupport(provider, providerID string, usage fantasy.Usage) message.CacheSupport {
	if usage.CacheReadTokens > 0 || usage.CacheCreationTokens > 0 {
		return message.CacheSupportNative
	}
	if provauth.KnownCacheReporter(provider) || promptcache.IsCacheReporterProviderID(providerID) {
		return message.CacheSupportNative
	}
	return message.CacheSupportNone
}

type MessageUsageStore interface {
	SetUsage(context.Context, string, message.TokenUsage) error
}

type MessageUsageHost interface {
	RecordMessageUsageStore() MessageUsageStore
}

type MessageUsageComponent struct{ host MessageUsageHost }

func NewMessageUsageComponent(host MessageUsageHost) *MessageUsageComponent {
	return &MessageUsageComponent{host: host}
}

func (m *MessageUsageComponent) RecordMessageUsage(
	ctx context.Context,
	messageID string,
	model agentcall.Model,
	usage fantasy.Usage,
	costDelta float64,
	estimated bool,
) {
	if messageID == "" {
		return
	}

	tu := message.TokenUsage{
		InputTokens:         usage.InputTokens,
		OutputTokens:        usage.OutputTokens,
		ReasoningTokens:     usage.ReasoningTokens,
		CacheCreationTokens: usage.CacheCreationTokens,
		CacheReadTokens:     usage.CacheReadTokens,
		TotalTokens:         usage.TotalTokens,
		CostUSD:             costDelta,
		// The EXECUTING model, matching the assistant message row itself
		// (agent_turn.go) and the two summarize paths, which have always
		// recorded it. These are the columns `rush sessions cache` and
		// UsageByModelInRange actually group by; leaving them on the
		// configured pair while the message row carried the executing one
		// split a single message's identity across two columns, which is
		// worse than the inconsistency that change set out to fix.
		Provider:     model.Model.Provider(),
		Model:        model.Model.Model(),
		CacheSupport: ProviderCacheSupport(model.Model.Provider(), model.ModelCfg.Provider, usage),
		Estimated:    estimated,
	}
	// An estimate is a guess derived from message lengths, not a measurement;
	// it can never be evidence about the cache.
	if estimated {
		tu.CacheSupport = message.CacheSupportNone
	}

	if tu.IsZero() {
		return
	}

	// context.WithoutCancel: usage is recorded at the tail of a turn, and the
	// turn's context is frequently already cancelled by then (user interrupt,
	// watchdog, cost cap). Inheriting that cancellation would drop the
	// statistics for exactly the turns whose accounting matters most — the
	// same failure mode as the cost-transfer bug fixed earlier in
	// coordinator.go.
	if err := m.host.RecordMessageUsageStore().SetUsage(context.WithoutCancel(ctx), messageID, tu); err != nil {
		slog.Warn(
			"agent: failed to record per-message usage",
			"messageID", messageID,
			"provider", tu.Provider,
			"model", tu.Model,
			"err", err,
		)
	}
}
