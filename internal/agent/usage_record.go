package agent

import (
	"context"
	"log/slog"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
)

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
func providerCacheSupport(provider string, usage fantasy.Usage) message.CacheSupport {
	if usage.CacheReadTokens > 0 || usage.CacheCreationTokens > 0 {
		return message.CacheSupportNative
	}
	if cacheProfileFor(provider).knownCacheReporter {
		return message.CacheSupportNative
	}
	return message.CacheSupportNone
}

// recordMessageUsage persists a single assistant message's token accounting.
//
// Deliberately best-effort: statistics must never abort or fail a turn, so a
// write error is logged and swallowed. The caller has already persisted the
// session-level cost/token snapshot by this point; this adds the per-message
// breakdown that makes cache analytics possible at all.
func (a *sessionAgent) recordMessageUsage(
	ctx context.Context,
	messageID string,
	model Model,
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
		CacheSupport: providerCacheSupport(model.Model.Provider(), usage),
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
	if err := a.messages.SetUsage(context.WithoutCancel(ctx), messageID, tu); err != nil {
		slog.Warn(
			"agent: failed to record per-message usage",
			"messageID", messageID,
			"provider", tu.Provider,
			"model", tu.Model,
			"err", err,
		)
	}
}
