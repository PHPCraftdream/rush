package agent

import (
	"testing"
	"time"

	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
)

func TestCacheProfileFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		want     cacheProfile
	}{
		{
			provider: anthropic.Name,
			want: cacheProfile{
				explicitMarkers:    true,
				keepAliveEligible:  true,
				ttl:                5 * time.Minute,
				knownCacheReporter: true,
			},
		},
		{
			provider: bedrock.Name,
			want: cacheProfile{
				explicitMarkers:    true,
				keepAliveEligible:  true,
				ttl:                5 * time.Minute,
				knownCacheReporter: true,
			},
		},
		{
			provider: vercel.Name,
			want: cacheProfile{
				explicitMarkers:    true,
				inputIncludesCache: true,
				keepAliveEligible:  true,
				ttl:                5 * time.Minute,
				knownCacheReporter: true,
			},
		},
		{
			provider: openrouter.Name,
			want: cacheProfile{
				inputIncludesCache: true,
				knownCacheReporter: true,
			},
		},
		{
			provider: google.Name,
			want: cacheProfile{
				inputIncludesCache: true,
				knownCacheReporter: true,
			},
		},
		{
			provider: openai.Name,
			want: cacheProfile{
				knownCacheReporter: true,
			},
		},
		{
			provider: cliprovider.ProviderID,
			want: cacheProfile{
				knownCacheReporter: true,
			},
		},
		{
			provider: "unknown-provider",
			want:     cacheProfile{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, cacheProfileFor(tt.provider))
		})
	}
}
