package agent

import (
	"encoding/json"
	"errors"
	"testing"
	"unicode/utf8"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestEffectiveMaxOutputTokens(t *testing.T) {
	tests := []struct {
		name     string
		model    Model
		override int64
		want     int64
	}{
		{
			name:     "positive call override takes precedence",
			model:    Model{ModelCfg: config.SelectedModel{MaxTokens: 700}, CatwalkCfg: catwalk.Model{DefaultMaxTokens: 900}},
			override: 300,
			want:     300,
		},
		{
			name:  "configured maximum",
			model: Model{ModelCfg: config.SelectedModel{MaxTokens: 700}, CatwalkCfg: catwalk.Model{DefaultMaxTokens: 900}},
			want:  700,
		},
		{
			name:     "nonpositive override ignored",
			model:    Model{ModelCfg: config.SelectedModel{MaxTokens: 700}},
			override: -1,
			want:     700,
		},
		{
			name:  "catalog default",
			model: Model{CatwalkCfg: catwalk.Model{DefaultMaxTokens: 900}},
			want:  900,
		},
		{
			name:     "unknown limits remain unknown",
			model:    Model{},
			override: -1,
			want:     0,
		},
		{
			name:     "negative configured value falls through",
			model:    Model{ModelCfg: config.SelectedModel{MaxTokens: -10}, CatwalkCfg: catwalk.Model{DefaultMaxTokens: 900}},
			override: 0,
			want:     900,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, effectiveMaxOutputTokens(tt.model, tt.override))
		})
	}
}

func TestContextInputBudget(t *testing.T) {
	tests := []struct {
		name          string
		contextWindow int64
		reserve       int64
		want          int64
	}{
		{name: "million-token context", contextWindow: 1_050_000, reserve: 128_000, want: 922_000},
		{name: "smaller override", contextWindow: 1_050_000, reserve: 300_000, want: 750_000},
		{name: "unknown context", contextWindow: 0, reserve: 128_000, want: 0},
		{name: "unknown reserve", contextWindow: 1_050_000, reserve: 0, want: 0},
		{name: "negative context", contextWindow: -1, reserve: 128_000, want: 0},
		{name: "negative reserve", contextWindow: 1_050_000, reserve: -1, want: 0},
		{name: "reserve fills context", contextWindow: 100, reserve: 100, want: 0},
		{name: "reserve exceeds context", contextWindow: 100, reserve: 101, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, contextInputBudget(tt.contextWindow, tt.reserve))
		})
	}
}

func TestContextInputBudgetIncludesCachedPromptUsageAndOutputReserve(t *testing.T) {
	const contextWindow, outputReserve int64 = 1_050_000, 128_000
	var sess session.Session
	updateSessionTokenCounters(&sess, fantasy.Usage{
		InputTokens:         400_000,
		CacheReadTokens:     100_000,
		CacheCreationTokens: 50_000,
		OutputTokens:        10,
	})

	budget := contextInputBudget(contextWindow, outputReserve)
	require.Equal(t, int64(550_000), sess.PromptTokens)
	require.Equal(t, int64(10), sess.CompletionTokens)
	require.Equal(t, int64(371_990), budget-sess.PromptTokens-sess.CompletionTokens)
}

func TestPreparedInputTokenCountUsesSerializedUTF8Bytes(t *testing.T) {
	text := "héllo 世界"
	prefix := "policy-前"
	messages := []fantasy.Message{{
		Role:    fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}},
	}}

	got, err := preparedInputTokenCount(messages, 128_000, prefix)
	require.NoError(t, err)
	encodedPart, err := json.Marshal(fantasy.TextPart{Text: text})
	require.NoError(t, err)
	encodedMessage, err := json.Marshal(struct {
		Role    fantasy.MessageRole `json:"role"`
		Content []json.RawMessage   `json:"content"`
	}{Role: fantasy.MessageRoleUser, Content: []json.RawMessage{encodedPart}})
	require.NoError(t, err)
	encodedPrefixPart, err := json.Marshal(fantasy.TextPart{Text: prefix})
	require.NoError(t, err)
	encodedPrefix, err := json.Marshal(struct {
		Role    fantasy.MessageRole `json:"role"`
		Content []json.RawMessage   `json:"content"`
	}{Role: fantasy.MessageRoleSystem, Content: []json.RawMessage{encodedPrefixPart}})
	require.NoError(t, err)
	encodedMessages, err := json.Marshal([]json.RawMessage{encodedPrefix, encodedMessage})
	require.NoError(t, err)
	require.Equal(t, int64(len(encodedMessages)), got)
	require.GreaterOrEqual(t, got, int64(utf8.RuneCountInString(text)+utf8.RuneCountInString(prefix)))

	withoutOutputReserve, err := preparedInputTokenCount(messages, 0, prefix)
	require.NoError(t, err)
	require.Equal(t, got, withoutOutputReserve, "reserve is budgeted separately, not in the input estimate")
}

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

	got, err := preparedInputTokenCount(messages, 128_000, "")
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

func TestPreparedInputTokenCountRejectsOpaqueMedia(t *testing.T) {
	_, err := preparedInputTokenCount([]fantasy.Message{{
		Role: fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{fantasy.FilePart{
			Filename: "image.png",
			Data:     []byte("opaque image bytes"),
		}},
	}}, 128_000, "")
	require.Error(t, err)
	require.ErrorContains(t, err, "opaque prompt content")
}

func TestPreparedInputTokenCountRejectsMediaToolResult(t *testing.T) {
	_, err := preparedInputTokenCount([]fantasy.Message{{
		Role: fantasy.MessageRoleTool,
		Content: []fantasy.MessagePart{fantasy.ToolResultPart{
			ToolCallID: "call-media",
			Output:     fantasy.ToolResultOutputContentMedia{MediaType: "image/png", Data: "opaque"},
		}},
	}}, 128_000, "")
	require.Error(t, err)
	require.ErrorContains(t, err, "unsupported tool result output")
}
