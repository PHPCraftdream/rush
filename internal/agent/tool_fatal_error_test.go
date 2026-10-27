package agent

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

type fatalToolTestInput struct{}

func TestFatalToolErrorStopsTurnAndIsRecorded(t *testing.T) {
	boom := errors.New("boom")
	failing := fantasy.NewAgentTool("failing", "fails", func(context.Context, fatalToolTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.ToolResponse{}, boom
	})
	ok := fantasy.NewAgentTool("ok", "succeeds", func(context.Context, fatalToolTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("fine"), nil
	})
	c := &fatalToolCollector{}
	tools := withFatalToolErrors([]fantasy.AgentTool{failing, ok}, c)
	require.Equal(t, "failing", tools[0].Info().Name)

	resp, err := tools[1].Run(context.Background(), fantasy.ToolCall{ID: "1", Name: "ok", Input: "{}"})
	require.NoError(t, err)
	require.False(t, resp.StopTurn)
	require.NoError(t, c.take())

	resp, err = tools[0].Run(context.Background(), fantasy.ToolCall{ID: "2", Name: "failing", Input: "{}"})
	require.NoError(t, err, "fantasy must not see the Go error")
	require.True(t, resp.IsError)
	require.True(t, resp.StopTurn, "the step must end the turn")
	require.ErrorIs(t, c.take(), boom)
	require.NoError(t, c.take(), "take clears the error for a retried attempt")
}
