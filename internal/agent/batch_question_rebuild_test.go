package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type rebuiltQuestionCaptureAgent struct {
	mockSessionAgent
	calls      int
	call       SessionAgentCall
	route      bool
	routeKnown bool
}

func (a *rebuiltQuestionCaptureAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	a.calls++
	a.call = call
	a.route, a.routeKnown = batchQuestionRouteFrom(ctx)
	return nil, nil
}

func TestRebuildLegacyChildCallRestoresNoQuestionRoute(t *testing.T) {
	coord, env := newControlCoordinator(t)
	parentID, childID := controlChild(t, env, "legacy-question-rebuild")
	seedControlDelegation(t, env, coord, parentID, childID)
	pinned, err := coord.resolveSessionModels(t.Context(), childID)
	require.NoError(t, err)
	original, err := coord.buildCall(t.Context(), childID, "old queued child", pinned, nil)
	require.NoError(t, err)
	original.batchQuestion = &batchChildQuestionState{Depth: 2, Route: false}
	encoded, err := json.Marshal(ToSessionAgentCallData(original))
	require.NoError(t, err)
	var data session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(encoded, &data))
	legacy, err := FromSessionAgentCallData(data)
	require.NoError(t, err)
	require.Nil(t, legacy.batchQuestion, "the fixture must reproduce a row with no process-local capability")

	rebuilt, err := coord.RebuildSessionAgentCall(t.Context(), "legacy-child-row", data)
	require.NoError(t, err)
	require.NotNil(t, rebuilt.batchQuestion)
	require.False(t, rebuilt.batchQuestion.Route)
	require.False(t, rebuilt.batchQuestion.BindingOK)
	capture := &rebuiltQuestionCaptureAgent{}
	coord.currentAgent = capture
	_, err = coord.RunSessionAgentCall(t.Context(), rebuilt)
	require.NoError(t, err)
	require.Equal(t, 1, capture.calls)
	require.True(t, capture.routeKnown)
	require.False(t, capture.route)
}

func TestRunLegacyChildCallWithoutRebuildCannotMintQuestionRoute(t *testing.T) {
	coord, env := newControlCoordinator(t)
	parentID, childID := controlChild(t, env, "legacy-question-consumer")
	seedControlDelegation(t, env, coord, parentID, childID)
	capture := &rebuiltQuestionCaptureAgent{}
	coord.currentAgent = capture
	_, err := coord.RunSessionAgentCall(t.Context(), SessionAgentCall{SessionID: childID})
	require.NoError(t, err)
	require.Equal(t, 1, capture.calls)
	require.NotNil(t, capture.call.batchQuestion)
	require.True(t, capture.routeKnown)
	require.False(t, capture.route)

	_, err = coord.RunSessionAgentCall(t.Context(), SessionAgentCall{SessionID: parentID})
	require.NoError(t, err)
	require.Equal(t, 2, capture.calls)
	require.Nil(t, capture.call.batchQuestion, "ordinary root queue calls retain their legacy contract")
	require.False(t, capture.routeKnown)
}

func TestRunRebuiltQuestionPolicyReadFailureDoesNotDispatch(t *testing.T) {
	coord, _ := newControlCoordinator(t)
	capture := &rebuiltQuestionCaptureAgent{}
	coord.currentAgent = capture
	_, err := coord.RunSessionAgentCall(t.Context(), SessionAgentCall{SessionID: "missing-child-session"})
	require.Error(t, err)
	require.ErrorContains(t, err, "resolve rebuilt question policy")
	require.Zero(t, capture.calls)
}

type questionRouteRecordingModel struct {
	fantasy.LanguageModel
	known atomic.Bool
	route atomic.Bool
}

func (m *questionRouteRecordingModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	route, known := batchQuestionRouteFrom(ctx)
	m.known.Store(known)
	m.route.Store(route)
	return m.LanguageModel.Stream(ctx, call)
}

func TestNoRouteTurnKeepsAskHiddenAfterSharedToolsRefresh(t *testing.T) {
	t.Setenv("RUSH_CACHE_KEEPALIVE", "false")
	env := testEnv(t)
	_, childID := controlChild(t, env, "question-tools-refresh")
	_, err := env.messages.Create(t.Context(), childID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "prior turn"}},
	})
	require.NoError(t, err)
	view := fantasy.NewAgentTool("view", "read", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("view"), nil
	})
	ask := fantasy.NewAgentTool(tools.AskQuestionToolName, "must stay hidden", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextErrorResponse("unexpected raw ask"), nil
	})
	inner := &toolSnapshotKeepAliveModel{provider: "question-probe", usage: fantasy.Usage{InputTokens: 100, OutputTokens: 10}}
	lm := &questionRouteRecordingModel{LanguageModel: inner}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("question tool refresh: provider_calls=%d route_known=%v route=%v", inner.callCount(), lm.known.Load(), lm.route.Load())
			if inner.callCount() > 0 {
				t.Logf("first provider tools: %v", keepAliveToolNames(inner.call(0)))
			}
		}
	})
	model := testKeepAliveModel(Model{Model: lm})
	a := NewSessionAgent(SessionAgentOptions{
		SmartModel: model, FastModel: model, SystemPrompt: "test", IsYolo: true,
		Sessions: env.sessions, Messages: env.messages, Tools: []fantasy.AgentTool{view},
	}).(*sessionAgent)
	t.Cleanup(func() { a.CancelAll() })
	previous := runTurnToolsSnapshotSeam
	runTurnToolsSnapshotSeam = func() { a.SetTools([]fantasy.AgentTool{view, ask}) }
	t.Cleanup(func() { runTurnToolsSnapshotSeam = previous })
	_, err = a.Run(t.Context(), SessionAgentCall{
		SessionID: childID, Prompt: "finish", NonInteractive: true,
		batchQuestion: &batchChildQuestionState{Depth: 1, Route: false},
	})
	require.NoError(t, err)
	require.Equal(t, 1, inner.callCount())
	require.True(t, lm.known.Load(), "runTurn must restore the route marker on the real provider context")
	require.False(t, lm.route.Load())
	require.Equal(t, []string{"view"}, keepAliveToolNames(inner.call(0)),
		"PrepareStep must not advertise raw ask from the refreshed shared toolset")
}
