package agent

import (
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
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
			got := providerCacheSupport(provider, fantasy.Usage{})
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

	got := providerCacheSupport("some-future-provider", fantasy.Usage{})
	require.Equal(t, message.CacheSupportNone, got)
}

// TestProviderCacheSupport_ObservedActivityIsAlwaysNative proves the
// unconditional fast path: any provider — known or not — reporting a nonzero
// cache counter is Native, since a real observation trumps list membership.
func TestProviderCacheSupport_ObservedActivityIsAlwaysNative(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		usage fantasy.Usage
	}{
		{"cache read", fantasy.Usage{CacheReadTokens: 10}},
		{"cache creation", fantasy.Usage{CacheCreationTokens: 10}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := providerCacheSupport("some-future-provider", tc.usage)
			require.Equal(t, message.CacheSupportNative, got)
		})
	}
}
