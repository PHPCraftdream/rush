// Single source of truth for per-provider prompt-cache capability, replacing
// the three scattered provider-name switches that used to encode this
// (getCacheControlOptions, cacheKeepAliveExplicitCacheProvider,
// normalizeProviderUsage) with one lookup table.
package provauth

import (
	"log/slog"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"

	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/provider/codexprovider"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/promptcache"
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
	codexprovider.Name: {
		// The provider decoder already makes input exclusive of cache reads.
		knownCacheReporter: true,
	},
}

// cacheProfileFor returns provider's cache profile, or the zero value
// (today's "unknown provider" behavior) if it is not listed.
func cacheProfileFor(provider string) cacheProfile {
	return cacheProfiles[provider]
}

// PromptCacheTTL reports the prompt-cache TTL the cache profiles assume
// for provider. Zero means the provider has no explicit TTL here
// (implicit-cache or unknown providers): callers must keep expiry unknown,
// not invent a TTL or read zero as "never expires".
//
// The per-provider table stays private here; the TTL lookup is delegated to
// the preexisting promptcache implementation with a lazy callback, evaluated
// at the read point — no eager snapshot of the table.
func PromptCacheTTL(provider string) time.Duration {
	return promptcache.PromptCacheTTL(provider, func(s string) time.Duration { return cacheProfileFor(s).ttl })
}

// ExplicitCacheControlProviders returns the fantasy provider names whose
// messages getCacheControlOptions should mark with an Anthropic-style
// ephemeral cache_control option.
func ExplicitCacheControlProviders() []string {
	var providers []string
	for provider, profile := range cacheProfiles {
		if profile.explicitMarkers {
			providers = append(providers, provider)
		}
	}
	return providers
}

// KeepAliveEligible reports whether scheduling an idle keep-alive replay can
// benefit provider — cacheKeepAliveExplicitCacheProvider's job.
func KeepAliveEligible(provider string) bool {
	return cacheProfileFor(provider).keepAliveEligible
}

// KnownCacheReporter reports whether provider is confirmed to emit cache
// counters at all — providerCacheSupport's job.
func KnownCacheReporter(provider string) bool {
	return cacheProfileFor(provider).knownCacheReporter
}

// NormalizeProviderUsage corrects fantasy.Usage for providers whose
// InputTokens is the provider's RAW prompt-token count, which already
// includes cached tokens, unlike anthropic/openai/bedrock where
// InputTokens is exclusive of the cache (see cliprovider/usage.go for the
// same convention-mismatch problem on the CLI side).
//
// Verified against fantasy v0.25.2 source: openrouter and vercel both map
// InputTokens: usage.PromptTokens (raw, OpenAI-shaped) and CacheReadTokens
// from prompt_tokens_details.cached_tokens, with CacheCreationTokens never
// set (language_model_hooks.go in each package). google maps InputTokens
// from PromptTokenCount (Gemini's inclusive counter) and CacheReadTokens
// from CachedContentTokenCount, always setting CacheCreationTokens: 0
// (google.go mapUsage). So for all three, only CacheReadTokens needs
// subtracting — CacheCreationTokens is never folded into their InputTokens.
// Which providers this applies to is centralized in cacheProfiles:
// inputIncludesCache.
func NormalizeProviderUsage(provider string, usage fantasy.Usage) fantasy.Usage {
	if !cacheProfileFor(provider).inputIncludesCache {
		return usage
	}

	input := usage.InputTokens - usage.CacheReadTokens
	if input < 0 {
		slog.Warn(
			"agent: provider reported more cached tokens than total input; clamping",
			"provider", provider,
			"input", usage.InputTokens,
			"cacheRead", usage.CacheReadTokens,
		)
		input = 0
	}
	usage.InputTokens = input
	return usage
}
