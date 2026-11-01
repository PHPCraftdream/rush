package agent

import (
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestPreparePromptPassesTargetFamilyForEmptyCompatReasoning(t *testing.T) {
	a := &sessionAgent{isSubAgent: true}
	msgs := []message.Message{{
		Role:     message.Assistant,
		Provider: openaicompat.Name,
		Parts: []message.ContentPart{
			message.ReasoningContent{},
			message.ToolCall{ID: "call_1", Name: "lookup", Input: `{}`},
		},
	}}

	history, _ := a.preparePrompt(msgs, nil, openaicompat.Name)
	require.Len(t, history, 2)
	require.Equal(t, fantasy.MessageRoleTool, history[1].Role)
	require.Len(t, history[0].Content, 2)
	_, ok := history[0].Content[0].(fantasy.ReasoningPart)
	require.True(t, ok)
	_, ok = history[0].Content[1].(fantasy.ToolCallPart)
	require.True(t, ok)

	openRouterHistory, _ := a.preparePrompt(msgs, nil, openrouter.Name)
	require.Len(t, openRouterHistory, 2)
	require.Equal(t, fantasy.MessageRoleTool, openRouterHistory[1].Role)
	require.Len(t, openRouterHistory[0].Content, 1)
	_, ok = openRouterHistory[0].Content[0].(fantasy.ToolCallPart)
	require.True(t, ok)
}
