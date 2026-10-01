package agent

import (
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func orderMsg(role message.MessageRole, parts ...message.ContentPart) message.Message {
	return message.Message{Role: role, Parts: parts}
}

func orderToolCall(id string) message.ToolCall {
	return message.ToolCall{ID: id, Name: "bash", Input: `{"command":"x"}`, Finished: true}
}

func orderToolResult(id string) message.ToolResult {
	return message.ToolResult{ToolCallID: id, Name: "bash", Content: "ok"}
}

func orderRoles(history []fantasy.Message) []fantasy.MessageRole {
	roles := make([]fantasy.MessageRole, 0, len(history))
	for _, m := range history {
		roles = append(roles, m.Role)
	}
	return roles
}

// A user message persisted while a tool ran (created_at between the assistant
// tool_use message and the tool message) must not sit between them in the
// provider history.
func TestPreparePrompt_UserInjectDuringToolRunFollowsTheToolRound(t *testing.T) {
	t.Parallel()
	a := &sessionAgent{isSubAgent: true}
	msgs := []message.Message{
		orderMsg(message.User, message.TextContent{Text: "start"}),
		orderMsg(message.Assistant, message.TextContent{Text: "running"}, orderToolCall("call_a")),
		orderMsg(message.User, message.TextContent{Text: "injected while the tool ran"}),
		orderMsg(message.Tool, orderToolResult("call_a")),
		orderMsg(message.Assistant, message.TextContent{Text: "done"}),
	}

	history, _ := a.preparePrompt(msgs, nil)

	require.Equal(t, []fantasy.MessageRole{
		fantasy.MessageRoleUser,
		fantasy.MessageRoleAssistant,
		fantasy.MessageRoleTool, // tool_result immediately after tool_use
		fantasy.MessageRoleUser, // the injection, after the round
		fantasy.MessageRoleAssistant,
	}, orderRoles(history))
}

// Two calls whose results arrive in two tool messages with an injection in
// between: the whole round stays contiguous.
func TestPreparePrompt_InjectBetweenTwoToolMessagesStaysAfterTheRound(t *testing.T) {
	t.Parallel()
	a := &sessionAgent{isSubAgent: true}
	msgs := []message.Message{
		orderMsg(message.Assistant, orderToolCall("call_a"), orderToolCall("call_b")),
		orderMsg(message.Tool, orderToolResult("call_a")),
		orderMsg(message.User, message.TextContent{Text: "injected"}),
		orderMsg(message.Tool, orderToolResult("call_b")),
	}

	history, _ := a.preparePrompt(msgs, nil)

	require.Equal(t, []fantasy.MessageRole{
		fantasy.MessageRoleAssistant,
		fantasy.MessageRoleTool,
		fantasy.MessageRoleTool,
		fantasy.MessageRoleUser,
	}, orderRoles(history))
}

// A round whose results never arrive keeps today's shape: the synthetic
// result sits right after the assistant message, the user message after it.
func TestPreparePrompt_UnansweredToolRoundIsLeftAlone(t *testing.T) {
	t.Parallel()
	a := &sessionAgent{isSubAgent: true}
	msgs := []message.Message{
		orderMsg(message.Assistant, orderToolCall("call_orphan")),
		orderMsg(message.User, message.TextContent{Text: "next"}),
	}

	history, _ := a.preparePrompt(msgs, nil)

	require.Equal(t, []fantasy.MessageRole{
		fantasy.MessageRoleAssistant,
		fantasy.MessageRoleTool, // synthetic
		fantasy.MessageRoleUser,
	}, orderRoles(history))
}

// Ordinary ordering is untouched: no injection, no change.
func TestDeferUserMessagesPastToolRound_NoOpWithoutInjection(t *testing.T) {
	t.Parallel()
	msgs := []message.Message{
		orderMsg(message.User, message.TextContent{Text: "a"}),
		orderMsg(message.Assistant, orderToolCall("c1")),
		orderMsg(message.Tool, orderToolResult("c1")),
		orderMsg(message.Assistant, message.TextContent{Text: "b"}),
		orderMsg(message.User, message.TextContent{Text: "c"}),
	}
	require.Equal(t, msgs, deferUserMessagesPastToolRound(msgs))
}
