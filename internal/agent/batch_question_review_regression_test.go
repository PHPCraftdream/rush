package agent

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent/state"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/stretchr/testify/require"
)

func TestQuestionRouteWakeWithoutDriverUsesDurableDelegationFacts(t *testing.T) {
	coord, env := newControlCoordinator(t)
	parentID, childID := controlChild(t, env, "question-wake-no-driver")
	seedControlDelegation(t, env, coord, parentID, childID)
	_, registered := coord.subAgentDrivers.Get(childID)
	require.False(t, registered, "the fixture must exercise the lost-driver wake branch")
	call, prep, err := coord.drainCallFor(t.Context(), childID)
	if prep != nil {
		defer prep.release()
	}
	require.NoError(t, err)
	require.NotNil(t, call.BatchQuestion)
	require.False(t, call.BatchQuestion.Route)
	require.False(t, call.BatchQuestion.BindingOK)
	ctx := batchmeta.ArmBatchQuestionTurnContext(t.Context(), call, state.WithQuestionContext, withBatchAgentQuestionBinding)
	route, known := batchmeta.BatchQuestionRouteFrom(ctx)
	require.True(t, known)
	require.False(t, route)
}

func TestQuestionRouteParentedForkRemainsAnOrdinarySession(t *testing.T) {
	coord, env := newControlCoordinator(t)
	parent, err := env.sessions.Create(t.Context(), "ordinary fork parent")
	require.NoError(t, err)
	fork, _, err := env.sessions.ForkSessionTx(t.Context(), parent.ID, session.ForkOptions{
		ParentID: parent.ID, Title: "ordinary child fork",
	})
	require.NoError(t, err)
	require.Equal(t, parent.ID, fork.ParentSessionID)
	capture := &rebuiltQuestionCaptureAgent{}
	coord.currentAgent = capture
	_, err = coord.RunSessionAgentCall(t.Context(), SessionAgentCall{SessionID: fork.ID})
	require.NoError(t, err)
	require.Equal(t, 1, capture.calls)
	require.Nil(t, capture.call.BatchQuestion)
	require.False(t, capture.routeKnown)
	call, prep, err := coord.drainCallFor(t.Context(), fork.ID)
	if prep != nil {
		defer prep.release()
	}
	require.NoError(t, err)
	require.Nil(t, call.BatchQuestion, "a parent link alone is not batch delegation authority")
}

func TestUpdateModelsDoesNotPublishOneChildQuestionRoute(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	recorder := &toolPublishRecorder{mockSessionAgent: newMockAgent("smart-provider", 4096,
		func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})}
	coord.currentAgent = recorder
	t.Cleanup(func() {
		if t.Failed() {
			published, models := recorder.snapshot()
			t.Logf("global question refresh: tool_publications=%v model_publications=%d", published, models)
		}
	})
	ctx := batchmeta.WithBatchQuestionRoute(t.Context(), false)
	ctx = state.WithQuestionContext(ctx, 2)
	ctx = withBatchAgentQuestionBinding(ctx, batchAgentQuestionBinding{Ask: func(context.Context, int, string) (json.RawMessage, error) {
		return nil, nil
	}})
	ctx = state.WithLifecycle(ctx, state.Lifecycle{})
	require.NoError(t, coord.UpdateModels(ctx))
	published, _ := recorder.snapshot()
	require.Len(t, published, 1)
	require.Contains(t, published[0], tools.AskQuestionToolName,
		"global refresh must preserve ordinary sessions' question tool")
	route, known := batchmeta.BatchQuestionRouteFrom(ctx)
	require.True(t, known)
	require.False(t, route, "the caller's own deny marker must remain unchanged")
}

func TestGlobalQuestionContextDropsBindingsWithoutChangingCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx = batchmeta.WithBatchQuestionRoute(ctx, false)
	ctx = state.WithQuestionContext(ctx, 2)
	ctx = withBatchAgentQuestionBinding(ctx, batchAgentQuestionBinding{Ask: func(context.Context, int, string) (json.RawMessage, error) {
		return nil, nil
	}})
	ctx = state.WithLifecycle(ctx, state.Lifecycle{})
	masked := withoutCallOptions(ctx)
	_, known := batchmeta.BatchQuestionRouteFrom(masked)
	require.False(t, known)
	_, bound := batchAgentQuestionBindingFrom(masked)
	require.False(t, bound)
	require.Zero(t, state.QuestionDepth(masked))
	require.Nil(t, masked.Value(state.LifecycleContextKey{}))
	cancel()
	require.ErrorIs(t, masked.Err(), context.Canceled)
}
