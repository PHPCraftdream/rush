package provauth

import (
	"testing"

	"charm.land/fantasy"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/stretchr/testify/require"
)

func TestContextInputBudget(t *testing.T) {
	tests := []struct {
		name          string
		contextWindow int64
		reserve       int64
		want          int64
	}{
		{name: "million-token context", contextWindow: 1_050_000, reserve: 128_000, want: 922_000},
		{name: "smaller override", contextWindow: 1_050_000, reserve: 300_000, want: 750_000},
		{name: "unknown context", contextWindow: 0, reserve: 128_000, want: 0},
		{name: "unknown reserve", contextWindow: 1_050_000, reserve: 0, want: 0},
		{name: "negative context", contextWindow: -1, reserve: 128_000, want: 0},
		{name: "negative reserve", contextWindow: 1_050_000, reserve: -1, want: 0},
		{name: "reserve fills context", contextWindow: 100, reserve: 100, want: 0},
		{name: "reserve exceeds context", contextWindow: 100, reserve: 101, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, contextInputBudget(tt.contextWindow, tt.reserve))
		})
	}
}

func TestContextInputBudgetIncludesCachedPromptUsageAndOutputReserve(t *testing.T) {
	const contextWindow, outputReserve int64 = 1_050_000, 128_000
	var sess session.Session
	UpdateSessionTokenCounters(&sess, fantasy.Usage{
		InputTokens:         400_000,
		CacheReadTokens:     100_000,
		CacheCreationTokens: 50_000,
		OutputTokens:        10,
	})

	budget := contextInputBudget(contextWindow, outputReserve)
	require.Equal(t, int64(550_000), sess.PromptTokens)
	require.Equal(t, int64(10), sess.CompletionTokens)
	require.Equal(t, int64(371_990), budget-sess.PromptTokens-sess.CompletionTokens)
}
