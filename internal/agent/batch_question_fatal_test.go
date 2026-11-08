package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestBatchQuestionRuntimeFailureEndsTurn(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	failure := errors.New("held question was cancelled and could not be resolved")
	ctx := withBatchAgentQuestionBinding(t.Context(), batchAgentQuestionBinding{Ask: func(context.Context, int, string) (json.RawMessage, error) { return nil, failure }})
	tool := batchAgentAskQuestionTool{}
	call := fantasy.ToolCall{Name: "ask_question", Input: `{"question":"continue?"}`}
	response, err := tool.Run(ctx, call)
	tl.addf("runtime ask failure: err=%v is_error=%v", err, response.IsError)
	require.ErrorIs(t, err, failure)
	collector := &fatalToolCollector{}
	wrapped := &fatalToolTool{inner: tool, collector: collector}
	response, err = wrapped.Run(ctx, call)
	tl.addf("fatal wrapper: err=%v is_error=%v stop_turn=%v", err, response.IsError, response.StopTurn)
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.True(t, response.StopTurn, "a removed hold must not be followed by another model step")
}

func TestBatchQuestionInvalidInputRemainsCorrectable(t *testing.T) {
	ctx := withBatchAgentQuestionBinding(t.Context(), batchAgentQuestionBinding{Ask: func(context.Context, int, string) (json.RawMessage, error) {
		t.Error("invalid input reached runtime hold")
		return nil, nil
	}})
	tool := batchAgentAskQuestionTool{}
	for _, input := range []string{`{`, `{}`} {
		response, err := tool.Run(ctx, fantasy.ToolCall{Name: "ask_question", Input: input})
		require.NoError(t, err)
		require.True(t, response.IsError)
		require.False(t, response.StopTurn)
	}
}
