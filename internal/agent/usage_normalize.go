package agent

import (
	"log/slog"

	"charm.land/fantasy"
)

// normalizeProviderUsage corrects fantasy.Usage for providers whose
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
//
// Which providers this applies to is centralized in cacheProfiles
// (cache_profile.go): inputIncludesCache.
func normalizeProviderUsage(provider string, usage fantasy.Usage) fantasy.Usage {
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
