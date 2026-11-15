package message

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMessageTruncateStreamedContent pins TruncateStreamedContent's exact
// scope: only the streamed text/reasoning parts go; tool calls, results and
// Finish stay, and a second call on an already-clean message is a no-op.
func TestMessageTruncateStreamedContent(t *testing.T) {
	m := &Message{Parts: []ContentPart{
		ReasoningContent{Thinking: "r", StartedAt: 1},
		TextContent{Text: "a"},
		ToolCall{ID: "c1", Name: "bash", Finished: true},
		ToolResult{ToolCallID: "c1", Content: "out"},
		TextContent{Text: "b"},
		Finish{Reason: FinishReasonUnknown, Partial: true},
	}}

	require.True(t, m.TruncateStreamedContent())
	require.Len(t, m.Parts, 3)
	require.IsType(t, ToolCall{}, m.Parts[0])
	require.IsType(t, ToolResult{}, m.Parts[1])
	require.IsType(t, Finish{}, m.Parts[2])
	require.Empty(t, m.FullText())
	require.Empty(t, m.ReasoningContent().Thinking)

	require.False(t, m.TruncateStreamedContent())
	require.Len(t, m.Parts, 3)
}
