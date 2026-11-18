package provauth

import (
	"encoding/json"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestPreparedInputTokenCountIncludesTypedToolContent(t *testing.T) {
	call := fantasy.ToolCallPart{ToolCallID: "call-123", ToolName: "lookup", Input: `{"query":"éclair"}`}
	result := fantasy.ToolResultPart{
		ToolCallID: call.ToolCallID,
		Output:     fantasy.ToolResultOutputContentText{Text: "résultat"},
	}
	errorResult := fantasy.ToolResultPart{
		ToolCallID: "call-error",
		Output:     fantasy.ToolResultOutputContentError{Error: errors.New("failed — détail")},
	}
	messages := []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{call, fantasy.ReasoningPart{Text: "reasoning trace"}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{result, errorResult}},
	}

	got, err := PreparedInputTokenCount(messages, 128_000, "")
	require.NoError(t, err)

	encodedCall, err := json.Marshal(call)
	require.NoError(t, err)
	encodedReasoning, err := json.Marshal(fantasy.ReasoningPart{Text: "reasoning trace"})
	require.NoError(t, err)
	encodedResult, err := json.Marshal(result)
	require.NoError(t, err)
	encodedError, err := serializedToolResult(errorResult)
	require.NoError(t, err)
	encodedMessages, err := json.Marshal([]json.RawMessage{
		encodeTestMessage(t, fantasy.MessageRoleAssistant, encodedCall, encodedReasoning),
		encodeTestMessage(t, fantasy.MessageRoleTool, encodedResult, encodedError),
	})
	require.NoError(t, err)
	require.Equal(t, int64(len(encodedMessages)), got)
	require.GreaterOrEqual(t, got, int64(len(call.Input)+len("résultat")+len("failed — détail")))
}

func encodeTestMessage(t *testing.T, role fantasy.MessageRole, content ...json.RawMessage) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Role    fantasy.MessageRole `json:"role"`
		Content []json.RawMessage   `json:"content"`
	}{Role: role, Content: content})
	require.NoError(t, err)
	return encoded
}
