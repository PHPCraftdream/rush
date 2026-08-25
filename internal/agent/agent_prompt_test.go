// Prompt-construction tests: preparePrompt orphaned-tool-use repair,
// workaroundProviderMediaLimitations media scrubbing, and the
// providerRetryLogFields / sanitizeToolInput guards.

package agent

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// Fork merge note: upstream's TestPreparePrompt_FiltersImageAttachments was
// removed at merge time — it tests the `supportsImages bool` parameter that
// we don't carry on preparePrompt(). Our equivalent scrub lives in
// workaroundProviderMediaLimitations() and is exercised by the higher-level
// streaming tests.

func TestPreparePrompt_OrphanedToolUse(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "test")
	require.NoError(t, err)

	// Create a user message.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "hello"},
		},
	})
	require.NoError(t, err)

	// Create an assistant message with a tool call but no tool result —
	// this simulates a cancelled/interrupted agent tool call.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "let me check"},
			message.ToolCall{
				ID:       "call_orphaned_1",
				Name:     "agent",
				Input:    `{"prompt":"do something"}`,
				Finished: true,
			},
		},
	})
	require.NoError(t, err)

	// Create the next user message (the one that interrupted the tool call).
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Fix #2"},
		},
	})
	require.NoError(t, err)

	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)

	history, _ := agent.preparePrompt(msgs, nil)

	// The history must contain a synthetic tool result for the orphaned call.
	found := false
	for _, msg := range history {
		if msg.Role != fantasy.MessageRoleTool {
			continue
		}
		for _, part := range msg.Content {
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if tr.ToolCallID == "call_orphaned_1" {
					found = true
					_, isError := tr.Output.(fantasy.ToolResultOutputContentError)
					require.True(t, isError, "orphaned tool result should be an error")
				}
			}
		}
	}
	require.True(t, found, "expected synthetic tool result for orphaned tool call")
}

func TestPreparePrompt_OrphanedToolUseMixed(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "test")
	require.NoError(t, err)

	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "hello"},
		},
	})
	require.NoError(t, err)

	// Assistant with 2 tool calls: one has a result, one is orphaned.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{
				ID:       "call_ok",
				Name:     "view",
				Input:    `{"path":"/foo"}`,
				Finished: true,
			},
			message.ToolCall{
				ID:       "call_orphaned",
				Name:     "agent",
				Input:    `{"prompt":"search"}`,
				Finished: true,
			},
		},
	})
	require.NoError(t, err)

	// Only one tool result — for call_ok.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "call_ok",
				Name:       "view",
				Content:    "file contents",
			},
		},
	})
	require.NoError(t, err)

	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)

	history, _ := agent.preparePrompt(msgs, nil)

	// Should have a synthetic result only for the orphaned call.
	var syntheticCount int
	for _, msg := range history {
		if msg.Role != fantasy.MessageRoleTool {
			continue
		}
		for _, part := range msg.Content {
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if tr.ToolCallID == "call_orphaned" {
					syntheticCount++
				}
			}
		}
	}
	require.Equal(t, 1, syntheticCount, "expected exactly one synthetic result for the orphaned call")
}

// TestPreparePrompt_ReminderPosition guards against the todo reminder sitting
// near the front of history: PrepareStep cache-marks the system message plus
// the last 2 messages, so a volatile reminder near the front busts the whole
// conversation's cache prefix on every todo mutation.
func TestPreparePrompt_ReminderPosition(t *testing.T) {
	newMsgs := func(t *testing.T, env fakeEnv, sessID string) []message.Message {
		t.Helper()
		ctx := t.Context()
		_, err := env.messages.Create(ctx, sessID, message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
		})
		require.NoError(t, err)
		_, err = env.messages.Create(ctx, sessID, message.CreateMessageParams{
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: "hi there"}},
		})
		require.NoError(t, err)
		msgs, err := env.messages.List(ctx, sessID)
		require.NoError(t, err)
		return msgs
	}

	hasReminder := func(msg fantasy.Message) bool {
		for _, part := range msg.Content {
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
				if strings.Contains(tp.Text, "<system_reminder>") {
					return true
				}
			}
		}
		return false
	}

	t.Run("empty todos - reminder is last, not first", func(t *testing.T) {
		env := testEnv(t)
		sa := testSessionAgent(env, nil, nil, "test prompt")
		agent := sa.(*sessionAgent)
		ctx := t.Context()
		sess, err := env.sessions.Create(ctx, "test")
		require.NoError(t, err)
		msgs := newMsgs(t, env, sess.ID)

		history, _ := agent.preparePrompt(msgs, nil)

		require.NotEmpty(t, history)
		require.False(t, hasReminder(history[0]), "reminder must not be the first message in history")
		last := history[len(history)-1]
		require.True(t, hasReminder(last), "reminder must be the last message in history")
		require.Contains(t, last.Content[0].(fantasy.TextPart).Text, "todo list is currently empty")

		// Every message derived from msgs must come strictly before the reminder.
		for _, m := range history[:len(history)-1] {
			require.False(t, hasReminder(m), "only the last message should carry the reminder")
		}
	})

	t.Run("non-empty todos - reminder is last, not first", func(t *testing.T) {
		env := testEnv(t)
		sa := testSessionAgent(env, nil, nil, "test prompt")
		agent := sa.(*sessionAgent)
		ctx := t.Context()
		sess, err := env.sessions.Create(ctx, "test")
		require.NoError(t, err)
		msgs := newMsgs(t, env, sess.ID)

		todos := []session.Todo{
			{Content: "write tests", Status: session.TodoStatusInProgress},
		}
		history, _ := agent.preparePrompt(msgs, todos)

		require.NotEmpty(t, history)
		require.False(t, hasReminder(history[0]), "reminder must not be the first message in history")
		last := history[len(history)-1]
		require.True(t, hasReminder(last), "reminder must be the last message in history")
		require.Contains(t, last.Content[0].(fantasy.TextPart).Text, "CURRENT todo list")
		require.Contains(t, last.Content[0].(fantasy.TextPart).Text, "write tests")
	})

	t.Run("sub-agent - no reminder appended at all", func(t *testing.T) {
		env := testEnv(t)
		sa := testSessionAgent(env, nil, nil, "test prompt")
		agent := sa.(*sessionAgent)
		agent.isSubAgent = true
		ctx := t.Context()
		sess, err := env.sessions.Create(ctx, "test")
		require.NoError(t, err)
		msgs := newMsgs(t, env, sess.ID)

		todos := []session.Todo{
			{Content: "write tests", Status: session.TodoStatusInProgress},
		}
		history, _ := agent.preparePrompt(msgs, todos)

		require.NotEmpty(t, history)
		for _, m := range history {
			require.False(t, hasReminder(m), "sub-agent history must never contain a todo reminder")
		}
	})
}

func TestWorkaroundProviderMediaLimitations_TextOnlyModel(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	pngBase64 := base64.StdEncoding.EncodeToString([]byte("fake-png-data"))

	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call_1",
					Output: fantasy.ToolResultOutputContentMedia{
						Data:      pngBase64,
						MediaType: "image/png",
					},
				},
			},
		},
	}

	// Non-Anthropic provider, no image support — should replace media with
	// a text placeholder and not create a synthetic user message.
	smartModel := Model{
		ModelCfg: config.SelectedModel{Provider: "openai"},
		CatwalkCfg: catwalk.Model{
			SupportsImages: false,
		},
	}

	result := agent.workaroundProviderMediaLimitations(messages, smartModel)

	// Should produce exactly one message: the tool message with a text
	// placeholder. No synthetic user message with FilePart.
	require.Len(t, result, 1)
	require.Equal(t, fantasy.MessageRoleTool, result[0].Role)

	tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](result[0].Content[0])
	require.True(t, ok)
	_, ok = fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Output)
	require.True(t, ok)
}

func TestWorkaroundProviderMediaLimitations_VisionModel(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	pngBase64 := base64.StdEncoding.EncodeToString([]byte("fake-png-data"))

	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call_1",
					Output: fantasy.ToolResultOutputContentMedia{
						Data:      pngBase64,
						MediaType: "image/png",
					},
				},
			},
		},
	}

	// Non-Anthropic provider, image support — should create a synthetic
	// user message with FilePart.
	smartModel := Model{
		ModelCfg: config.SelectedModel{Provider: "openai"},
		CatwalkCfg: catwalk.Model{
			SupportsImages: true,
		},
	}

	result := agent.workaroundProviderMediaLimitations(messages, smartModel)

	// Should produce two messages: tool message with placeholder text,
	// and synthetic user message with FilePart.
	require.Len(t, result, 2)
	require.Equal(t, fantasy.MessageRoleTool, result[0].Role)
	require.Equal(t, fantasy.MessageRoleUser, result[1].Role)

	// The tool message should have text placeholder.
	tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](result[0].Content[0])
	require.True(t, ok)
	textOutput, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Output)
	require.True(t, ok)
	require.Contains(t, textOutput.Text, "see attached file")

	// The synthetic user message should contain a TextPart and a FilePart.
	require.Len(t, result[1].Content, 2)
	file, ok := fantasy.AsMessagePart[fantasy.FilePart](result[1].Content[1])
	require.True(t, ok)
	require.Equal(t, "image/png", file.MediaType)
}

func TestWorkaroundProviderMediaLimitations_AnthropicProvider(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	pngBase64 := base64.StdEncoding.EncodeToString([]byte("fake-png-data"))

	messages := []fantasy.Message{
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{
					ToolCallID: "call_1",
					Output: fantasy.ToolResultOutputContentMedia{
						Data:      pngBase64,
						MediaType: "image/png",
					},
				},
			},
		},
	}

	// Anthropic provider — should return messages unchanged regardless of
	// SupportsImages, since Anthropic handles media in tool results natively.
	smartModel := Model{
		ModelCfg: config.SelectedModel{Provider: string(catwalk.InferenceProviderAnthropic)},
		CatwalkCfg: catwalk.Model{
			SupportsImages: true,
		},
	}

	result := agent.workaroundProviderMediaLimitations(messages, smartModel)
	require.Len(t, result, 1)
	require.Equal(t, fantasy.MessageRoleTool, result[0].Role)

	// The media should still be in the tool result, untouched.
	tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](result[0].Content[0])
	require.True(t, ok)
	media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](tr.Output)
	require.True(t, ok)
	require.Equal(t, "image/png", media.MediaType)
}

func TestProviderRetryLogFields(t *testing.T) {
	t.Run("nil provider error", func(t *testing.T) {
		fields := providerRetryLogFields(nil, 2*time.Second)
		require.Equal(t, []any{"retry_delay", "2s"}, fields)
	})

	t.Run("provider error with title and message", func(t *testing.T) {
		fields := providerRetryLogFields(&fantasy.ProviderError{
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
		fields := providerRetryLogFields(&fantasy.ProviderError{
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
		out, sanitized := sanitizeToolInput("view", "call_1", input)
		require.Equal(t, input, out)
		require.False(t, sanitized)
	})

	t.Run("malformed JSON is replaced with empty object", func(t *testing.T) {
		out, sanitized := sanitizeToolInput("view", "call_2", `{"path":"foo.go"`)
		require.Equal(t, "{}", out)
		require.True(t, sanitized)
	})

	t.Run("empty string is sanitized", func(t *testing.T) {
		out, sanitized := sanitizeToolInput("view", "call_3", "")
		require.Equal(t, "{}", out)
		require.True(t, sanitized)
	})
}
