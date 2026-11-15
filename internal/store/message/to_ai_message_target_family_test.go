package message

import (
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"github.com/stretchr/testify/require"
)

func TestEmptyOpenAICompatReasoningRoundTripTargets(t *testing.T) {
	original := Message{
		Role:     Assistant,
		Provider: openaicompat.Name,
		Parts: []ContentPart{
			ReasoningContent{},
			ToolCall{ID: "call_1", Name: "lookup", Input: `{}`},
		},
	}
	encoded, err := marshalParts(original.Parts)
	require.NoError(t, err)
	decoded, err := unmarshalParts(encoded)
	require.NoError(t, err)
	original.Parts = decoded

	tests := []struct {
		name         string
		targetFamily string
		wantReason   bool
	}{
		{name: "compat", targetFamily: openaicompat.Name, wantReason: true},
		{name: "openrouter", targetFamily: openrouter.Name},
		{name: "anthropic", targetFamily: anthropic.Name},
		{name: "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := original.ToAIMessageFor(tt.targetFamily)
			require.Len(t, messages, 1)
			wantContent := 1
			if tt.wantReason {
				wantContent++
			}
			require.Len(t, messages[0].Content, wantContent)
			index := 0
			if tt.wantReason {
				reasoning, ok := messages[0].Content[0].(fantasy.ReasoningPart)
				require.True(t, ok)
				require.Equal(t, "", reasoning.Text)
				require.Empty(t, reasoning.ProviderOptions)
				index++
			}
			call, ok := messages[0].Content[index].(fantasy.ToolCallPart)
			require.True(t, ok)
			require.Equal(t, "call_1", call.ToolCallID)
		})
	}

	genericMessages := original.ToAIMessage()
	require.Len(t, genericMessages, 1)
	require.Len(t, genericMessages[0].Content, 1)
	_, ok := genericMessages[0].Content[0].(fantasy.ToolCallPart)
	require.True(t, ok)
}

func TestEmptyOpenAICompatReasoningFromOtherProviderDrops(t *testing.T) {
	m := Message{Role: Assistant, Provider: anthropic.Name, Parts: []ContentPart{
		ReasoningContent{},
		ToolCall{ID: "call_1", Name: "lookup", Input: `{}`},
	}}
	messages := m.ToAIMessageFor(openaicompat.Name)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].Content, 1)
	_, ok := messages[0].Content[0].(fantasy.ToolCallPart)
	require.True(t, ok)
}

func TestEmptyOpenAICompatReasoningWithoutToolCallDoesNotInventReasoning(t *testing.T) {
	m := Message{Role: Assistant, Provider: openaicompat.Name, Parts: []ContentPart{
		ToolCall{ID: "call_1", Name: "lookup", Input: `{}`},
	}}
	messages := m.ToAIMessageFor(openaicompat.Name)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].Content, 1)
	_, ok := messages[0].Content[0].(fantasy.ToolCallPart)
	require.True(t, ok)
}

func TestOpenAICompatMapperKeepsEmptyReasoningAlongsideToolCall(t *testing.T) {
	m := Message{Role: Assistant, Provider: openaicompat.Name, Parts: []ContentPart{
		ReasoningContent{},
		ToolCall{ID: "call_1", Name: "lookup", Input: `{}`},
	}}
	prompt := m.ToAIMessageFor(openaicompat.Name)
	converted, warnings := openaicompat.ToPromptFunc(prompt, "", "")
	require.Empty(t, warnings)
	encoded, err := json.Marshal(converted)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"reasoning_content":""`)
	require.Contains(t, string(encoded), `"tool_calls"`)
}

func TestOpenRouterMapperAcceptsEmptyReasoningToolCallHistory(t *testing.T) {
	m := Message{Role: Assistant, Provider: openaicompat.Name, Parts: []ContentPart{
		ReasoningContent{},
		ToolCall{ID: "call_1", Name: "lookup", Input: `{}`},
	}}
	openRouterPrompt := m.ToAIMessageFor(openrouter.Name)
	converted, warnings := openrouter.ToPrompt(openRouterPrompt, "", "openai/test")
	require.NotContains(t, strings.Join(warningMessages(warnings), " "), "does not have the right type")
	encoded, err := json.Marshal(converted)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), `\u003cthoughts\u003e`)

	compatPrompt := m.ToAIMessageFor(openaicompat.Name)
	compatConverted, warnings := openrouter.ToPrompt(compatPrompt, "", "openai/test")
	require.Empty(t, warnings)
	compatEncoded, err := json.Marshal(compatConverted)
	require.NoError(t, err)
	require.Contains(t, string(compatEncoded), `\u003cthoughts\u003e`)
}

func warningMessages(warnings []fantasy.CallWarning) []string {
	messages := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		messages = append(messages, warning.Message)
	}
	return messages
}
