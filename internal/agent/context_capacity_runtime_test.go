package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func newContextCapacityRuntimeStream(model Model, call SessionAgentCall, usage fantasy.Usage) *turnStream {
	var currentSession session.Session
	updateSessionTokenCounters(&currentSession, usage)
	return &turnStream{
		a:              &sessionAgent{},
		smartModel:     model,
		call:           call,
		currentSession: currentSession,
	}
}

func TestTurnStreamStopConditionsIncludesDefaultOutputReserve(t *testing.T) {
	const contextWindow, outputReserve int64 = 1_050_000, 128_000
	model := Model{CatwalkCfg: catwalk.Model{
		ContextWindow:    contextWindow,
		DefaultMaxTokens: outputReserve,
	}}
	ts := newContextCapacityRuntimeStream(model, SessionAgentCall{}, fantasy.Usage{
		InputTokens:     141,
		CacheReadTokens: 921_344,
		OutputTokens:    117,
	})

	// The cached-inclusive prompt counter makes remaining budget 398 tokens.
	// The legacy 20k-only window threshold would not fire here.
	require.Equal(t, int64(921_485), ts.currentSession.PromptTokens)
	require.Equal(t, int64(117), ts.currentSession.CompletionTokens)
	budget := contextInputBudget(contextWindow, outputReserve)
	require.Equal(t, int64(398), budget-ts.currentSession.PromptTokens-ts.currentSession.CompletionTokens)

	conditions := ts.stopConditions()
	require.NotEmpty(t, conditions)
	require.True(t, conditions[0](nil))
	require.True(t, ts.shouldSummarize)
}

func TestTurnStreamStopConditionsUsesConfiguredOutputOverride(t *testing.T) {
	const contextWindow int64 = 1_050_000
	model := Model{
		CatwalkCfg: catwalk.Model{
			ContextWindow:    contextWindow,
			DefaultMaxTokens: 128_000,
		},
		ModelCfg: config.SelectedModel{MaxTokens: 300_000},
	}
	ts := newContextCapacityRuntimeStream(model, SessionAgentCall{}, fantasy.Usage{
		InputTokens:  729_990,
		OutputTokens: 10,
	})

	// The configured 300k maximum leaves exactly the summarize threshold;
	// using the catalog's 128k default instead would leave 192k.
	budget := contextInputBudget(contextWindow, effectiveMaxOutputTokens(model, 0))
	require.Equal(t, int64(20_000), budget-ts.currentSession.PromptTokens-ts.currentSession.CompletionTokens)
	conditions := ts.stopConditions()
	require.True(t, conditions[0](nil))
	require.True(t, ts.shouldSummarize)
}

func TestTurnStreamStopConditionsKeepsLegacyGateWithoutOutputLimit(t *testing.T) {
	const contextWindow int64 = 1_050_000
	model := Model{CatwalkCfg: catwalk.Model{ContextWindow: contextWindow}}
	ts := newContextCapacityRuntimeStream(model, SessionAgentCall{}, fantasy.Usage{
		InputTokens:  1_029_990,
		OutputTokens: 10,
	})

	conditions := ts.stopConditions()
	require.True(t, conditions[0](nil))
	require.True(t, ts.shouldSummarize)
}

func TestTurnStreamStopConditionsSkipsUnknownContextWindow(t *testing.T) {
	ts := newContextCapacityRuntimeStream(Model{CatwalkCfg: catwalk.Model{DefaultMaxTokens: 128_000}}, SessionAgentCall{}, fantasy.Usage{
		InputTokens:  1_029_990,
		OutputTokens: 10,
	})

	conditions := ts.stopConditions()
	require.False(t, conditions[0](nil))
	require.False(t, ts.shouldSummarize)
}
