package agent

import (
	"encoding/json"
	"testing"
	"unicode/utf8"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
	"github.com/PHPCraftdream/rush/internal/config"
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
			require.Equal(t, tt.want, provauth.EffectiveMaxOutputTokens(tt.model, tt.override))
		})
	}
}

func TestPreparedInputTokenCountUsesSerializedUTF8Bytes(t *testing.T) {
	text := "héllo 世界"
	prefix := "policy-前"
	messages := []fantasy.Message{{
		Role:    fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}},
	}}

	got, err := provauth.PreparedInputTokenCount(messages, 128_000, prefix)
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

	withoutOutputReserve, err := provauth.PreparedInputTokenCount(messages, 0, prefix)
	require.NoError(t, err)
	require.Equal(t, got, withoutOutputReserve, "reserve is budgeted separately, not in the input estimate")
}

func TestPreparedInputTokenCountRejectsOpaqueMedia(t *testing.T) {
	_, err := provauth.PreparedInputTokenCount([]fantasy.Message{{
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
	_, err := provauth.PreparedInputTokenCount([]fantasy.Message{{
		Role: fantasy.MessageRoleTool,
		Content: []fantasy.MessagePart{fantasy.ToolResultPart{
			ToolCallID: "call-media",
			Output:     fantasy.ToolResultOutputContentMedia{MediaType: "image/png", Data: "opaque"},
		}},
	}}, 128_000, "")
	require.Error(t, err)
	require.ErrorContains(t, err, "unsupported tool result output")
}
