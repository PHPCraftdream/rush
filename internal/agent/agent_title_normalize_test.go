package agent

import (
	"context"
	"errors"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"
	"github.com/stretchr/testify/require"
)

// titleUsageMockModel streams a fixed title text plus a caller-controlled
// Usage, so a test can pin the exact cost generateTitle computes from it.
type titleUsageMockModel struct {
	provider string
	usage    fantasy.Usage
}

func (m *titleUsageMockModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not implemented")
}

func (m *titleUsageMockModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	usage := m.usage
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "0"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "0", Delta: "My Title"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "0"}) {
			return
		}
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        usage,
		})
	}, nil
}

func (m *titleUsageMockModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *titleUsageMockModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *titleUsageMockModel) Provider() string { return m.provider }
func (*titleUsageMockModel) Model() string      { return "title-usage-mock" }

// TestGenerateTitle_NormalizesInclusiveProviderUsageBeforeCosting proves the
// fix for a real gap found by the 2026-08-26 review: generateTitle used to
// cost resp.TotalUsage directly, double-counting cached tokens for
// OpenRouter/Vercel/Google exactly like the main turn did before
// normalizeProviderUsage existed. With a raw (cache-inclusive) InputTokens of
// 1000 and CacheReadTokens of 900, the correct (normalized) prompt cost must
// price only 100 fresh input tokens, not 1000.
func TestGenerateTitle_NormalizesInclusiveProviderUsageBeforeCosting(t *testing.T) {
	catwalkCfg := catwalk.Model{
		ContextWindow:    200000,
		DefaultMaxTokens: 1000,
		CostPer1MIn:      1.0,
		CostPer1MOut:     2.0,
	}
	model := Model{
		Model: &titleUsageMockModel{
			provider: openrouter.Name,
			usage: fantasy.Usage{
				InputTokens:     1000, // raw, inclusive of the 900 cached
				CacheReadTokens: 900,
				OutputTokens:    10,
			},
		},
		CatwalkCfg: catwalkCfg,
	}

	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "")
	require.NoError(t, err)

	a, ok := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)
	require.True(t, ok)

	cfg := turnConfig{fastModel: model, smartModel: model}
	a.generateTitle(t.Context(), sess.ID, "first message", cfg)

	updated, err := env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "My Title", updated.Title)

	// Normalized: InputTokens corrected to 100 (1000 - 900 cache read) before
	// costing. CostPer1MIn=1.0 -> 100/1e6*1.0 = 0.0001. CostPer1MOut=2.0 ->
	// 10/1e6*2.0 = 0.00002. Total = 0.00012.
	//
	// The old (unnormalized) behavior would have priced the full 1000 raw
	// input tokens: 1000/1e6*1.0 = 0.001, ten times too high.
	require.InDelta(t, 0.00012, updated.Cost, 1e-9,
		"title cost must be computed from normalized (not raw, cache-inclusive) usage")
}
