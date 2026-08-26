// Single source of truth for per-provider prompt-cache capability, replacing
// the three scattered provider-name switches that used to encode this
// (getCacheControlOptions, cacheKeepAliveExplicitCacheProvider,
// normalizeProviderUsage) with one lookup table.
package agent

import (
	"time"

	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
)

// cacheProfile is one provider's prompt-cache behavior. The zero value is the
// safe default for any provider not listed in cacheProfiles: no explicit
// markers, no keep-alive, InputTokens treated as already exclusive of cache
// (anthropic/openai/bedrock's convention, not vercel/openrouter/google's
// inclusive one), and NOT a known cache reporter — see knownCacheReporter.
type cacheProfile struct {
	// explicitMarkers: getCacheControlOptions should mark this provider's
	// messages with an Anthropic-style ephemeral cache_control option.
	explicitMarkers bool
	// inputIncludesCache: normalizeProviderUsage must subtract
	// CacheReadTokens from InputTokens for this provider.
	inputIncludesCache bool
	// keepAliveEligible: cacheKeepAliveExplicitCacheProvider's job — whether
	// scheduling an idle keep-alive replay can benefit this provider.
	keepAliveEligible bool
	// ttl is the cache TTL this provider's marker implies. 0 means
	// unknown/not applicable.
	ttl time.Duration
	// knownCacheReporter: providerCacheSupport's job — whether this provider
	// is confirmed to emit cache counters at all (regardless of whether it
	// also needs any of the other, deviation-only facets above). A provider
	// NOT in this list is not "assumed native" on a zero-counter turn; it is
	// classified as unknown ("n/a"), not a fabricated 0%.
	knownCacheReporter bool
}

// cacheProfiles is keyed by fantasy provider name (e.g. anthropic.Name).
// Vercel appears with BOTH explicitMarkers and inputIncludesCache set: it is
// a router whose upstream model is Anthropic-style explicit-cache today, but
// its wire usage shape is still the raw (cache-inclusive) OpenAI-style count
// — both facts are independently true and preserved as-is.
//
// openai.Name and cliprovider.ProviderID ("local-cli") are listed here ONLY
// for knownCacheReporter — neither needs explicit markers, inclusive-input
// correction, or keep-alive (openai's own fantasy hooks already subtract
// cached tokens from InputTokens; local-cli's CLIs are normalized in
// cliprovider/usage.go, a separate mechanism, before usage ever reaches this
// package).
var cacheProfiles = map[string]cacheProfile{
	anthropic.Name: {
		explicitMarkers:    true,
		keepAliveEligible:  true,
		ttl:                5 * time.Minute,
		knownCacheReporter: true,
	},
	bedrock.Name: {
		explicitMarkers:    true,
		keepAliveEligible:  true,
		ttl:                5 * time.Minute,
		knownCacheReporter: true,
	},
	vercel.Name: {
		explicitMarkers:    true,
		inputIncludesCache: true,
		keepAliveEligible:  true,
		ttl:                5 * time.Minute,
		knownCacheReporter: true,
	},
	openrouter.Name: {
		inputIncludesCache: true,
		knownCacheReporter: true,
	},
	google.Name: {
		inputIncludesCache: true,
		knownCacheReporter: true,
	},
	openai.Name: {
		knownCacheReporter: true,
	},
	cliprovider.ProviderID: {
		knownCacheReporter: true,
	},
}

// cacheProfileFor returns provider's cache profile, or the zero value
// (today's "unknown provider" behavior) if it is not listed.
func cacheProfileFor(provider string) cacheProfile {
	return cacheProfiles[provider]
}
