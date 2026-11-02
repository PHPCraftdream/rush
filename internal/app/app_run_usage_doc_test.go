package app

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestRunUsageInfoPreservesMixedCacheCountersAndProviderTotals(t *testing.T) {
	first := message.TokenUsage{
		InputTokens: 20, CacheReadTokens: 80, OutputTokens: 5, TotalTokens: 90,
		CacheSupport: message.CacheSupportNative,
	}
	second := message.TokenUsage{
		InputTokens: 10, CacheReadTokens: 30, OutputTokens: 7, TotalTokens: 40,
		CacheSupport: message.CacheSupportNone,
	}
	aggregated := first.Add(second)
	report := message.UsageReport{
		ByModel: []message.ModelUsage{{Messages: 2, Usage: aggregated}},
	}

	got := buildSessionUsageInfo(report)
	require.NotNil(t, got)
	require.Equal(t, int64(110), got.CacheReadTokens)
	require.Equal(t, "none", got.CacheSupport)
	require.Equal(t, int64(140), got.PromptTokens)
	require.Equal(t, int64(130), got.TotalTokens)
	require.Less(t, got.TotalTokens, got.PromptTokens)
}
