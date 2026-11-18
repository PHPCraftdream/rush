// PURE leaf tests relocated from internal/agent: provider retry log
// fields, tool-input sanitization, user-message deferral ordering,
// and superseded supervision notice replacement.

package sessionprompt

import (
	"log/slog"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
	"github.com/PHPCraftdream/rush/internal/store/message"
	_ "github.com/joho/godotenv/autoload"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	slog.SetLogLoggerLevel(slog.LevelError)
	testguard.Main(m)
}

func orderMsg(role message.MessageRole, parts ...message.ContentPart) message.Message {
	return message.Message{Role: role, Parts: parts}
}

func orderToolCall(id string) message.ToolCall {
	return message.ToolCall{ID: id, Name: "bash", Input: `{"command":"x"}`, Finished: true}
}

func orderToolResult(id string) message.ToolResult {
	return message.ToolResult{ToolCallID: id, Name: "bash", Content: "ok"}
}

func idsOf(msgs []message.Message) []string {
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	return ids
}

func TestProviderRetryLogFields(t *testing.T) {
	t.Run("nil provider error", func(t *testing.T) {
		fields := ProviderRetryLogFields(nil, 2*time.Second)
		require.Equal(t, []any{"retry_delay", "2s"}, fields)
	})

	t.Run("provider error with title and message", func(t *testing.T) {
		fields := ProviderRetryLogFields(&fantasy.ProviderError{
			StatusCode: 429,
			Title:      "rate limit",
			Message:    "too many requests",
		}, 1500*time.Millisecond)
		require.Equal(t, []any{
			"retry_delay", "1.5s",
			"status_code", 429,
			"title", "rate limit",
			"message", "too many requests",
		}, fields)
	})

	t.Run("provider error without optional strings", func(t *testing.T) {
		fields := ProviderRetryLogFields(&fantasy.ProviderError{
			StatusCode: 503,
		}, time.Second)
		require.Equal(t, []any{
			"retry_delay", "1s",
			"status_code", 503,
		}, fields)
	})
}

// TestSanitizeToolInput pins the JSON validation guard that prevents
// malformed tool call arguments from a provider (e.g. truncated or garbled
// JSON) from getting persisted verbatim and bricking the session on replay.
func TestSanitizeToolInput(t *testing.T) {
	t.Run("valid JSON is returned unchanged", func(t *testing.T) {
		input := `{"path":"foo.go","limit":10}`
		out, sanitized := SanitizeToolInput("view", "call_1", input)
		require.Equal(t, input, out)
		require.False(t, sanitized)
	})

	t.Run("malformed JSON is replaced with empty object", func(t *testing.T) {
		out, sanitized := SanitizeToolInput("view", "call_2", `{"path":"foo.go"`)
		require.Equal(t, "{}", out)
		require.True(t, sanitized)
	})

	t.Run("empty string is sanitized", func(t *testing.T) {
		out, sanitized := SanitizeToolInput("view", "call_3", "")
		require.Equal(t, "{}", out)
		require.True(t, sanitized)
	})
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
	require.Equal(t, msgs, DeferUserMessagesPastToolRound(msgs))
}

// TestDropSupersededSupervisionNotices: only the newest NoticeKind
// "supervision" message keeps its text; older ones stay in place (role
// alternation is preserved) with a short marker; everything else is untouched.
func TestDropSupersededSupervisionNotices(t *testing.T) {
	t.Parallel()
	mk := func(id, noticeKind, text string) message.Message {
		return message.Message{
			ID: id, Role: message.User, NoticeKind: noticeKind,
			Parts: []message.ContentPart{message.TextContent{Text: text}},
		}
	}
	in := []message.Message{
		mk("1", "", "hello"),
		mk("2", NoticeKindSupervision, "tick 1"),
		mk("3", "job_stopped", "stopped"),
		mk("4", NoticeKindSupervision, "tick 2"),
		mk("5", NoticeKindSupervision, "tick 3 paused"),
		mk("6", "", "bye"),
	}
	out := DropSupersededSupervisionNotices(in)

	require.Equal(t, []string{"1", "2", "3", "4", "5", "6"}, idsOf(out), "no message is removed")
	texts := make([]string, len(out))
	for i, m := range out {
		texts[i] = m.Content().Text
	}
	require.Equal(t, []string{"hello", supersededSupervisionText, "stopped", supersededSupervisionText, "tick 3 paused", "bye"}, texts)
	require.Equal(t, "tick 1", in[1].Content().Text, "the input slice must not be mutated")
}
