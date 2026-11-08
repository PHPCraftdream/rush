package agent

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestQuestionRouteWakeWithoutDriverUsesDurableDelegationFacts(t *testing.T) {
	coord, env := newControlCoordinator(t)
	parentID, childID := controlChild(t, env, "question-wake-no-driver")
	seedControlDelegation(t, env, coord, parentID, childID)
	_, registered := coord.subAgentDrivers.get(childID)
	require.False(t, registered, "the fixture must exercise the lost-driver wake branch")
	call, prep, err := coord.drainCallFor(t.Context(), childID)
	if prep != nil {
		defer prep.release()
	}
	require.NoError(t, err)
	require.NotNil(t, call.batchQuestion)
	require.False(t, call.batchQuestion.Route)
	require.False(t, call.batchQuestion.BindingOK)
	ctx := armBatchQuestionTurnContext(t.Context(), call)
	route, known := batchQuestionRouteFrom(ctx)
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
	require.Nil(t, capture.call.batchQuestion)
	require.False(t, capture.routeKnown)
	call, prep, err := coord.drainCallFor(t.Context(), fork.ID)
	if prep != nil {
		defer prep.release()
	}
	require.NoError(t, err)
	require.Nil(t, call.batchQuestion, "a parent link alone is not batch delegation authority")
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
	ctx := withBatchQuestionRoute(t.Context(), false)
	ctx = withBatchAgentQuestionContext(ctx, 2)
	ctx = withBatchAgentQuestionBinding(ctx, batchAgentQuestionBinding{Ask: func(context.Context, int, string) (json.RawMessage, error) {
		return nil, nil
	}})
	ctx = withBatchAgentLifecycle(ctx, batchAgentLifecycle{})
	require.NoError(t, coord.UpdateModels(ctx))
	published, _ := recorder.snapshot()
	require.Len(t, published, 1)
	require.Contains(t, published[0], tools.AskQuestionToolName,
		"global refresh must preserve ordinary sessions' question tool")
	route, known := batchQuestionRouteFrom(ctx)
	require.True(t, known)
	require.False(t, route, "the caller's own deny marker must remain unchanged")
}

func TestGlobalQuestionContextDropsBindingsWithoutChangingCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx = withBatchQuestionRoute(ctx, false)
	ctx = withBatchAgentQuestionContext(ctx, 2)
	ctx = withBatchAgentQuestionBinding(ctx, batchAgentQuestionBinding{Ask: func(context.Context, int, string) (json.RawMessage, error) {
		return nil, nil
	}})
	ctx = withBatchAgentLifecycle(ctx, batchAgentLifecycle{})
	masked := withoutCallOptions(ctx)
	_, known := batchQuestionRouteFrom(masked)
	require.False(t, known)
	_, bound := batchAgentQuestionBindingFrom(masked)
	require.False(t, bound)
	require.Zero(t, batchAgentQuestionDepth(masked))
	require.Nil(t, masked.Value(batchAgentLifecycleContextKey{}))
	cancel()
	require.ErrorIs(t, masked.Err(), context.Canceled)
}
