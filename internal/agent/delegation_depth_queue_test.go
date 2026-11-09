package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestQueuedCallDepthReadsLatestDurableCap(t *testing.T) {
	env := testEnv(t)
	root, err := env.sessions.Create(t.Context(), "queued depth parent")
	require.NoError(t, err)
	store := env.sessions.(session.AgentDepthStore)
	require.NoError(t, store.LowerAgentDepth(t.Context(), root.ID, DefaultAgentDepth))
	original := &CallOptions{
		MaxAgentDepth: DefaultAgentDepth, AgentDepthSet: true,
		MaxCost: 7, AllowPeakHours: true,
	}
	call := SessionAgentCall{SessionID: root.ID, CallOptions: original}
	// The operator lowers the durable cap after the call was queued.
	require.NoError(t, store.LowerAgentDepth(t.Context(), root.ID, 0))
	ctx, remaining, err := withPersistedCallAgentDepth(t.Context(), env.sessions, &call)
	require.NoError(t, err)
	require.Zero(t, remaining)
	require.NotNil(t, call.CallOptions)
	require.NotSame(t, original, call.CallOptions)
	require.Zero(t, call.CallOptions.MaxAgentDepth)
	require.True(t, call.CallOptions.AgentDepthSet)
	require.Equal(t, original.MaxCost, call.CallOptions.MaxCost)
	require.Equal(t, original.AllowPeakHours, call.CallOptions.AllowPeakHours)
	require.Equal(t, DefaultAgentDepth, original.MaxAgentDepth, "tightening cannot mutate a shared queued template")
	require.Same(t, call.CallOptions, callOptionsFrom(ctx))
}

func TestLegacyQueuedCallDepthPreservesNilOptions(t *testing.T) {
	env := testEnv(t)
	root, err := env.sessions.Create(t.Context(), "legacy queued depth")
	require.NoError(t, err)
	store := env.sessions.(session.AgentDepthStore)
	require.NoError(t, store.LowerAgentDepth(t.Context(), root.ID, 0))
	call := SessionAgentCall{SessionID: root.ID}
	ctx, remaining, err := withPersistedCallAgentDepth(t.Context(), env.sessions, &call)
	require.NoError(t, err)
	require.Zero(t, remaining)
	require.Nil(t, call.CallOptions)
	require.Nil(t, callOptionsFrom(ctx), "durable depth alone must not change legacy admission semantics")
	effective, err := MaxAgentDepthFrom(ctx)
	require.NoError(t, err)
	require.Zero(t, effective)
}

func TestQueuedCallDepthRefusesMissingSession(t *testing.T) {
	env := testEnv(t)
	call := SessionAgentCall{SessionID: "missing-depth-session"}
	_, _, err := withPersistedCallAgentDepth(t.Context(), env.sessions, &call)
	require.ErrorContains(t, err, "lineage unavailable")
}
