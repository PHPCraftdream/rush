package message

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"github.com/stretchr/testify/require"
)

func TestEmptyReasoningSurvivesPartsCodecAndToAIMessage(t *testing.T) {
	m := &Message{Role: Assistant}
	m.AppendReasoningContent("")
	m.AppendReasoningSignature("anthropic-signature")
	m.AppendThoughtSignature("google-signature", "google-tool-id")
	m.FinishThinking()

	parts, err := marshalParts(m.Parts)
	require.NoError(t, err)
	decoded, err := unmarshalParts(parts)
	require.NoError(t, err)
	m.Parts = decoded
	reasoningContent := m.ReasoningContent()
	require.Equal(t, "google-signature", reasoningContent.ThoughtSignature)
	require.Equal(t, "google-tool-id", reasoningContent.ToolID)

	ai := m.ToAIMessage()
	require.Len(t, ai, 1)
	require.Len(t, ai[0].Content, 1)
	reasoning, ok := ai[0].Content[0].(fantasy.ReasoningPart)
	require.True(t, ok)
	require.Empty(t, reasoning.Text)
	require.Equal(t, "anthropic-signature", reasoning.ProviderOptions[anthropic.Name].(*anthropic.ReasoningOptionMetadata).Signature)
	googleMetadata, ok := reasoning.ProviderOptions[google.Name].(*google.ReasoningMetadata)
	require.True(t, ok)
	require.Equal(t, "google-signature", googleMetadata.Signature)
	require.Equal(t, "google-tool-id", googleMetadata.ToolID)

	user := (&Message{Role: User, Parts: []ContentPart{TextContent{Text: "Continue."}}}).ToAIMessage()
	require.Len(t, user, 1)
	ai = append(ai, user...)
	require.Len(t, ai, 2)
	require.Equal(t, fantasy.MessageRoleUser, ai[1].Role)
}

func TestReasoningPartsRequireTextOrMetadata(t *testing.T) {
	responsesData := &openai.ResponsesReasoningMetadata{ItemID: "rs_metadata"}
	tests := []struct {
		name             string
		thinking         string
		setMetadata      func(*Message)
		expectedProvider string
	}{
		{
			name: "empty without metadata",
		},
		{
			name:             "anthropic signature",
			setMetadata:      func(m *Message) { m.AppendReasoningSignature("anthropic-signature") },
			expectedProvider: anthropic.Name,
		},
		{
			name:             "google thought signature",
			setMetadata:      func(m *Message) { m.AppendThoughtSignature("google-signature", "google-tool-id") },
			expectedProvider: google.Name,
		},
		{
			name:        "responses data only",
			setMetadata: func(m *Message) { m.SetReasoningResponsesData(responsesData) },
		},
		{
			name:     "nonempty text",
			thinking: "reasoning text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Message{Role: Assistant}
			m.AppendReasoningContent(tt.thinking)
			if tt.setMetadata != nil {
				tt.setMetadata(m)
			}

			messages := m.ToAIMessage()
			var reasoningParts []fantasy.ReasoningPart
			for _, message := range messages {
				for _, part := range message.Content {
					if reasoning, ok := part.(fantasy.ReasoningPart); ok {
						reasoningParts = append(reasoningParts, reasoning)
					}
				}
			}

			// fantasy skips reasoning on Responses replay, so this metadata has no wire effect.
			// Responses API metadata alone must not create a fantasy reasoning part.
			wantCount := 0
			if tt.expectedProvider != "" || tt.thinking != "" {
				wantCount = 1
			}
			require.Len(t, reasoningParts, wantCount)
			if wantCount == 0 {
				return
			}

			reasoning := reasoningParts[0]
			require.Equal(t, tt.thinking, reasoning.Text)
			switch tt.expectedProvider {
			case anthropic.Name:
				metadata, ok := reasoning.ProviderOptions[anthropic.Name].(*anthropic.ReasoningOptionMetadata)
				require.True(t, ok)
				require.Equal(t, "anthropic-signature", metadata.Signature)
				require.NotContains(t, reasoning.ProviderOptions, google.Name)
			case google.Name:
				metadata, ok := reasoning.ProviderOptions[google.Name].(*google.ReasoningMetadata)
				require.True(t, ok)
				require.Equal(t, "google-signature", metadata.Signature)
				require.Equal(t, "google-tool-id", metadata.ToolID)
				require.NotContains(t, reasoning.ProviderOptions, anthropic.Name)
			default:
				require.Empty(t, reasoning.ProviderOptions)
			}
		})
	}
}

func TestEmptyReasoningPreservedOnAnthropicWire(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-3-5-sonnet-20241022\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	m := &Message{Role: Assistant}
	m.AppendReasoningContent("")
	m.AppendReasoningSignature("signed-empty-thinking")
	m.FinishThinking()
	m.AppendContent("ok")
	parts, err := marshalParts(m.Parts)
	require.NoError(t, err)
	m.Parts, err = unmarshalParts(parts)
	require.NoError(t, err)
	messages := m.ToAIMessage()
	messages = append(messages, (&Message{Role: User, Parts: []ContentPart{TextContent{Text: "Continue."}}}).ToAIMessage()...)

	provider, err := anthropic.New(anthropic.WithBaseURL(server.URL), anthropic.WithAPIKey("test"))
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "claude-3-5-sonnet-20241022")
	require.NoError(t, err)
	stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: messages})
	require.NoError(t, err)
	for range stream {
	}

	apiMessages, ok := body["messages"].([]any)
	require.True(t, ok)
	require.Len(t, apiMessages, 2)
	content, ok := apiMessages[0].(map[string]any)["content"].([]any)
	require.True(t, ok)
	var thinkingFound, textFound bool
	for _, item := range content {
		part := item.(map[string]any)
		switch part["type"] {
		case "thinking":
			thinkingFound = true
			require.Contains(t, part, "thinking")
			require.Equal(t, "", part["thinking"])
			require.Equal(t, "signed-empty-thinking", part["signature"])
		case "text":
			textFound = true
			require.Equal(t, "ok", part["text"])
		}
	}
	require.True(t, thinkingFound)
	require.True(t, textFound)
	require.Equal(t, "user", apiMessages[1].(map[string]any)["role"])
}

func TestEmptyReasoningPreservedOnGoogleGenerateWire(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
	}))
	defer server.Close()

	m := &Message{Role: Assistant}
	m.AppendReasoningContent("")
	m.AppendThoughtSignature("google-empty-thought-signature", "call-1")
	m.FinishThinking()
	m.AddToolCall(ToolCall{ID: "call-1", Name: "lookup", Input: `{"q":"x"}`})
	parts, err := marshalParts(m.Parts)
	require.NoError(t, err)
	m.Parts, err = unmarshalParts(parts)
	require.NoError(t, err)
	prompt := m.ToAIMessage()
	prompt = append(prompt, (&Message{Role: User, Parts: []ContentPart{TextContent{Text: "Continue."}}}).ToAIMessage()...)

	provider, err := google.New(google.WithBaseURL(server.URL), google.WithGeminiAPIKey("test"))
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gemini-2.5-flash")
	require.NoError(t, err)
	_, err = model.Generate(t.Context(), fantasy.Call{Prompt: prompt})
	require.NoError(t, err)

	contents, ok := body["contents"].([]any)
	require.True(t, ok)
	require.Len(t, contents, 2)
	require.Equal(t, "model", contents[0].(map[string]any)["role"])
	assistantParts := contents[0].(map[string]any)["parts"].([]any)
	var functionCall map[string]any
	for _, item := range assistantParts {
		part := item.(map[string]any)
		if call, ok := part["functionCall"].(map[string]any); ok {
			functionCall = part
			require.Equal(t, "lookup", call["name"])
		}
	}
	require.NotNil(t, functionCall)
	encodedSignature, ok := functionCall["thoughtSignature"].(string)
	require.True(t, ok)
	signature, err := base64.StdEncoding.DecodeString(encodedSignature)
	require.NoError(t, err)
	// The signature must survive even without thought text: it carries opaque context the model needs to continue the tool call.
	// Google carries the thought signature on the functionCall part as bytes encoded in JSON.
	require.Equal(t, "google-empty-thought-signature", string(signature))
	userParts := contents[1].(map[string]any)["parts"].([]any)
	require.Equal(t, "Continue.", userParts[0].(map[string]any)["text"])
}
