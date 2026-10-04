package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T12b: the per-call smart override wins over the session row when both are
// present. The reviewer pass arms its own slot's effort as a non-persisted
// ModelOverride while the session row still names the executor's effort, so
// a message row must record the override.
//
// Revert-check: making turnSmartReasoningEffort always return
// sess.SmartModelReasoningEffort makes the "override wins" case below fail.
func TestTurnSmartReasoningEffort(t *testing.T) {
	tests := []struct {
		name     string
		ctx      context.Context
		sess     session.Session
		expected string
	}{
		{
			name:     "override wins",
			ctx:      WithModelOverrides(t.Context(), &ModelOverride{ReasoningEffort: "max"}, nil),
			sess:     session.Session{SmartModelReasoningEffort: "medium"},
			expected: "max",
		},
		{
			name:     "no override uses the session",
			ctx:      t.Context(),
			sess:     session.Session{SmartModelReasoningEffort: "medium"},
			expected: "medium",
		},
		{
			name:     "override with an empty effort falls back to the session",
			ctx:      WithModelOverrides(t.Context(), &ModelOverride{ReasoningEffort: ""}, nil),
			sess:     session.Session{SmartModelReasoningEffort: "high"},
			expected: "high",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, turnSmartReasoningEffort(tt.ctx, tt.sess))
		})
	}
}

// The FAST slot's effort must never leak into the answer: only the smart
// override is consulted, so a fast-only call still records the session's
// smart effort.
func TestTurnSmartReasoningEffort_FastOverrideIsIgnored(t *testing.T) {
	ctx := WithModelOverrides(t.Context(), nil, &ModelOverride{ReasoningEffort: "low"})
	sess := session.Session{SmartModelReasoningEffort: "medium"}

	assert.Equal(t, "medium", turnSmartReasoningEffort(ctx, sess))
}
