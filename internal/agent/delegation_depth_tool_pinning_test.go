package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDepthToolPinningOnlyChangesExhaustedSurface(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts CallOptions
		want bool
	}{
		{name: "legacy options"},
		{name: "zero", opts: CallOptions{AgentDepthSet: true}, want: true},
		{name: "one", opts: CallOptions{AgentDepthSet: true, MaxAgentDepth: 1}},
		{name: "default", opts: CallOptions{AgentDepthSet: true, MaxAgentDepth: DefaultAgentDepth}},
		{name: "maximum", opts: CallOptions{AgentDepthSet: true, MaxAgentDepth: MaxAgentDepthForResume}},
		{name: "other policy still pins", opts: CallOptions{AgentDepthSet: true, MaxAgentDepth: DefaultAgentDepth, DisableSubAgents: true}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, scopedCallOptionsRequireDistinctTools(&tc.opts))
		})
	}
}

func TestLegacyExhaustedDepthMarkerRequiresPinnedTools(t *testing.T) {
	t.Parallel()
	ctx := context.WithValue(t.Context(), persistedAgentDepthContextKey{}, 0)
	require.Nil(t, callOptionsFrom(ctx))
	require.True(t, scopedCallToolsRequired(ctx), "nil options cannot hide the exhausted durable cap from a tool-build failure")
	require.False(t, scopedCallToolsRequired(t.Context()), "control: legacy default-depth calls retain their fallback")
}
