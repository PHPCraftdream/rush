package agent

import (
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	"github.com/stretchr/testify/require"
)

func TestNormalizeProviderUsage_SubtractsCacheReadForInclusiveProviders(t *testing.T) {
	t.Parallel()

	for _, provider := range []string{openrouter.Name, vercel.Name, google.Name} {
		provider := provider
		t.Run(provider, func(t *testing.T) {
			t.Parallel()

			usage := fantasy.Usage{
				InputTokens:     1000,
				CacheReadTokens: 900,
				OutputTokens:    50,
			}
			got := normalizeProviderUsage(provider, usage)
			require.Equal(t, int64(100), got.InputTokens)
			require.Equal(t, int64(900), got.CacheReadTokens)
			require.Equal(t, int64(50), got.OutputTokens)
		})
	}
}

func TestNormalizeProviderUsage_PassthroughForExclusiveProviders(t *testing.T) {
	t.Parallel()

	for _, provider := range []string{anthropic.Name, openai.Name, "unknown-provider"} {
		provider := provider
		t.Run(provider, func(t *testing.T) {
			t.Parallel()

			usage := fantasy.Usage{
				InputTokens:         1000,
				CacheReadTokens:     900,
				CacheCreationTokens: 200,
				OutputTokens:        50,
			}
			got := normalizeProviderUsage(provider, usage)
			require.Equal(t, usage, got)
		})
	}
}

func TestNormalizeProviderUsage_ClampsWhenCacheReadExceedsInput(t *testing.T) {
	t.Parallel()

	usage := fantasy.Usage{
		InputTokens:     100,
		CacheReadTokens: 900,
	}
	got := normalizeProviderUsage(openrouter.Name, usage)
	require.Equal(t, int64(0), got.InputTokens)
}
