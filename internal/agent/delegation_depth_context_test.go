package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestPersistedAgentDepthPreservesLegacyCallOptions(t *testing.T) {
	for _, cap := range []int{0, 1, DefaultAgentDepth} {
		t.Run(fmt.Sprintf("remaining-%d", cap), func(t *testing.T) {
			env := testEnv(t)
			sess, err := env.sessions.Create(t.Context(), "legacy depth context")
			require.NoError(t, err)
			store, ok := env.sessions.(session.AgentDepthStore)
			require.True(t, ok)
			require.NoError(t, store.LowerAgentDepth(t.Context(), sess.ID, cap))
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx, err := WithPersistedSessionAgentDepth(parent, env.sessions, sess.ID)
			require.NoError(t, err)
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("legacy depth fixture: session_id=%s cap=%d explicit_options=%v", sess.ID, cap, callOptionsFrom(ctx) != nil)
				}
			})
			require.Nil(t, callOptionsFrom(ctx), "legacy admission and shared-limit branches must retain nil options")
			got, err := MaxAgentDepthFrom(ctx)
			require.NoError(t, err)
			require.Equal(t, cap, got)
			got, err = effectiveParentAgentDepth(ctx, env.sessions, sess.ID)
			require.NoError(t, err)
			require.Equal(t, cap, got)
			attemptedLift, err := WithAgentDepth(ctx, MaxAgentDepthForResume)
			require.NoError(t, err)
			got, err = MaxAgentDepthFrom(attemptedLift)
			require.NoError(t, err)
			require.Equal(t, cap, got, "a later options wrapper cannot erase the durable cap")

			global := withoutCallOptions(ctx)
			require.Nil(t, callOptionsFrom(global))
			got, err = MaxAgentDepthFrom(global)
			require.NoError(t, err)
			require.Equal(t, DefaultAgentDepth, got, "a single session's depth cannot shape shared model tools")
			cancel()
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			require.ErrorIs(t, global.Err(), context.Canceled)
		})
	}
}

func TestPersistedAgentDepthHonorsExplicitRootAndClonesOptions(t *testing.T) {
	env := testEnv(t)
	root, err := env.sessions.Create(t.Context(), "explicit root depth")
	require.NoError(t, err)
	original := &CallOptions{
		MaxAgentDepth: MaxAgentDepthForResume, AgentDepthSet: true,
		MaxCost: 7, AllowPeakHours: true,
	}
	ctx, err := WithPersistedSessionAgentDepth(WithCallOptions(t.Context(), original), env.sessions, root.ID)
	require.NoError(t, err)
	got := callOptionsFrom(ctx)
	require.NotNil(t, got)
	require.NotSame(t, original, got)
	require.Equal(t, MaxAgentDepthForResume, got.MaxAgentDepth, "a new explicit root is not silently reduced to the default")
	require.True(t, got.AgentDepthSet)
	require.Equal(t, original.MaxCost, got.MaxCost)
	require.Equal(t, original.AllowPeakHours, got.AllowPeakHours)

	store, ok := env.sessions.(session.AgentDepthStore)
	require.True(t, ok)
	require.NoError(t, store.LowerAgentDepth(t.Context(), root.ID, 1))
	ctx, err = WithPersistedSessionAgentDepth(WithCallOptions(t.Context(), original), env.sessions, root.ID)
	require.NoError(t, err)
	require.Equal(t, 1, callOptionsFrom(ctx).MaxAgentDepth)
	require.Equal(t, MaxAgentDepthForResume, original.MaxAgentDepth, "tightening cannot mutate another call's options")
}
