package agent

import (
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/message"
)

// TestProviderCacheSupport_KnownReportersStayNativeOnZeroCounters pins the
// exact "no behavior change for currently-supported providers" requirement:
// every provider cacheProfiles lists as a knownCacheReporter must still
// report Native on a zero-counter turn, matching the old always-Native
// fallback exactly for these providers.
func TestProviderCacheSupport_KnownReportersStayNativeOnZeroCounters(t *testing.T) {
	t.Parallel()

	knownReporters := []string{
		anthropic.Name, bedrock.Name, vercel.Name,
		openrouter.Name, google.Name, openai.Name,
		cliprovider.ProviderID,
	}
	for _, provider := range knownReporters {
		provider := provider
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			got := providerCacheSupport(provider, "", fantasy.Usage{})
			require.Equal(t, message.CacheSupportNative, got)
		})
	}
}

// TestProviderCacheSupport_UnknownProviderIsNoneOnZeroCounters is the new
// behavior: a provider not in cacheProfiles' knownCacheReporter list, with no
// observed cache activity, is an honest "n/a" (CacheSupportNone) instead of a
// fabricated 0% (the old unconditional CacheSupportNative fallback).
func TestProviderCacheSupport_UnknownProviderIsNoneOnZeroCounters(t *testing.T) {
	t.Parallel()

	got := providerCacheSupport(openaicompat.Name, "local-glm", fantasy.Usage{})
	require.Equal(t, message.CacheSupportNone, got)
}

// TestProviderCacheSupport_ObservedActivityIsAlwaysNative proves the
// unconditional fast path: any provider — known or not — reporting a nonzero
// cache counter is Native, since a real observation trumps list membership.
func TestProviderCacheSupport_ObservedActivityIsAlwaysNative(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		providerID string
		usage      fantasy.Usage
	}{
		{"cache read, unknown id", "local-glm", fantasy.Usage{CacheReadTokens: 10}},
		{"cache creation, unknown id", "local-glm", fantasy.Usage{CacheCreationTokens: 10}},
		{"cache read, zai", "zai", fantasy.Usage{CacheReadTokens: 10}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := providerCacheSupport(openaicompat.Name, tc.providerID, tc.usage)
			require.Equal(t, message.CacheSupportNative, got)
		})
	}
}

// TestProviderCacheSupport_OpenAICompatByProviderID is the table oracle for
// #1139: the fantasy openai-compat type serves both cache-reporting endpoints
// (zai) and cache-silent local ones, so classification must fall back to the
// CONFIGURED provider ID, not the wire type. zai with zero counters is native
// (real GLM sessions open with ~6 zero-counter turns; without the ID fallback
// TokenUsage.Add degrades the whole session to CacheSupportNone and the cache
// hit ratio vanishes). An unlisted openai-compat ID stays None.
func TestProviderCacheSupport_OpenAICompatByConfiguredProviderID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		provider   string
		providerID string
		usage      fantasy.Usage
		want       message.CacheSupport
	}{
		{
			name:       "zai zero counters is native",
			provider:   openaicompat.Name,
			providerID: "zai",
			usage:      fantasy.Usage{},
			want:       message.CacheSupportNative,
		},
		{
			name:       "unlisted openai-compat id zero counters is none",
			provider:   openaicompat.Name,
			providerID: "my-local-endpoint",
			usage:      fantasy.Usage{},
			want:       message.CacheSupportNone,
		},
		{
			name:       "unlisted id on a known type stays native",
			provider:   openai.Name,
			providerID: "my-local-endpoint",
			usage:      fantasy.Usage{},
			want:       message.CacheSupportNative,
		},
		{
			name:       "zai with observed counters is native",
			provider:   openaicompat.Name,
			providerID: "zai",
			usage:      fantasy.Usage{CacheReadTokens: 130000},
			want:       message.CacheSupportNative,
		},
		{
			name:       "empty id openai-compat zero counters is none",
			provider:   openaicompat.Name,
			providerID: "",
			usage:      fantasy.Usage{},
			want:       message.CacheSupportNone,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := providerCacheSupport(tc.provider, tc.providerID, tc.usage)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestTokenUsageAdd_ZaiWarmupTurnsKeepCacheHitRatio pins the end-to-end
// consequence of #1139: a zai session whose first turns carried zero cache
// counters must still aggregate to Native with a working CacheHitRatio once
// the cache kicks in — the classification by provider ID has to remove the
// "worst participant" degradation Add used to apply.

func TestTokenUsageAdd_ZaiWarmupTurnsKeepCacheHitRatio(t *testing.T) {
	t.Parallel()

	// Six warm-up turns (zai reports no cache counters on them) followed by
	// warm turns with the real ~2.4K input / ~130K cache-read shape.
	support := providerCacheSupport(openaicompat.Name, "zai", fantasy.Usage{})
	total := message.TokenUsage{}
	for range 6 {
		total = total.Add(message.TokenUsage{
			InputTokens:  2400,
			OutputTokens: 500,
			CacheSupport: support,
		})
	}
	total = total.Add(message.TokenUsage{
		InputTokens:     2400,
		OutputTokens:    500,
		CacheReadTokens: 130000,
		CacheSupport:    providerCacheSupport(openaicompat.Name, "zai", fantasy.Usage{CacheReadTokens: 130000}),
	})

	require.Equal(t, message.CacheSupportNative, total.CacheSupport)
	ratio, ok := total.CacheHitRatio()
	require.True(t, ok)
	require.InDelta(t, float64(130000)/float64(130000+6*2400+2400), ratio, 1e-9)
}
