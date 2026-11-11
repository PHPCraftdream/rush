package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	fantasyopenai "charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/nettransport"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func publicResponsesHistoryDecode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value map[string]any
	require.NoError(t, decoder.Decode(&value))
	return value
}

func publicResponsesHistoryCanonical(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var canonical any
	require.NoError(t, decoder.Decode(&canonical))
	return canonical
}

func publicResponsesHistoryCapture[T any](t *testing.T, ctx context.Context, capture <-chan T, label string) T {
	t.Helper()
	select {
	case value := <-capture:
		return value
	case <-ctx.Done():
		t.Fatalf("waiting for %s: %v", label, ctx.Err())
	}
	var zero T
	return zero
}

func TestPublicResponsesWebSocketHistoryMatchesFantasyHTTPComposition(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	const argumentText = `{ "n" : 9007199254740993 }`
	argumentJSON := strings.ReplaceAll(argumentText, `"`, `\"`)
	const messageItemID = "msg-history-prior"
	const reasoningItemID = "rs-history-prior"
	const functionItemID = "fc-history-prior"
	const functionCallID = "call-history-prior"
	const assistantText = "prior assistant answer"
	const reasoningSummary = "prior private reasoning summary"
	const encryptedReasoning = "opaque-prior-encrypted-content"

	messageOutput := `{"type":"message","id":"` + messageItemID + `","role":"assistant","status":"completed","content":[{"type":"output_text","text":"` + assistantText + `"}]}`
	reasoningOutput := `{"type":"reasoning","id":"` + reasoningItemID + `","summary":[{"type":"summary_text","text":"` + reasoningSummary + `"}],"encrypted_content":"` + encryptedReasoning + `"}`
	functionOutput := `{"type":"function_call","id":"` + functionItemID + `","call_id":"` + functionCallID + `","name":"lookup","arguments":"` + argumentJSON + `"}`
	responseOutput := `[` + messageOutput + `,` + reasoningOutput + `,` + functionOutput + `]`
	events := []string{
		`{"type":"response.created","response":{"id":"resp-history-prior","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"` + messageItemID + `","role":"assistant","status":"in_progress","content":[]}}`,
		`{"type":"response.output_text.delta","item_id":"` + messageItemID + `","output_index":0,"content_index":0,"delta":"` + assistantText + `"}`,
		`{"type":"response.output_item.done","output_index":0,"item":` + messageOutput + `}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"reasoning","id":"` + reasoningItemID + `","summary":[],"encrypted_content":"` + encryptedReasoning + `"}}`,
		`{"type":"response.reasoning_summary_part.added","item_id":"` + reasoningItemID + `","output_index":1,"summary_index":0,"part":{"type":"summary_text","text":""}}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"` + reasoningItemID + `","output_index":1,"summary_index":0,"delta":"` + reasoningSummary + `"}`,
		`{"type":"response.output_item.done","output_index":1,"item":` + reasoningOutput + `}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"` + functionItemID + `","call_id":"` + functionCallID + `","name":"lookup","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"` + functionItemID + `","delta":"` + argumentJSON + `"}`,
		`{"type":"response.output_item.done","output_index":2,"item":` + functionOutput + `}`,
		`{"type":"response.completed","response":{"id":"resp-history-prior","status":"completed","output":` + responseOutput + `}}`,
	}

	wsFrames := make(chan []byte, 2)
	referenceBodies := make(chan []byte, 1)
	var upgrades, wsRequests, referenceHTTP, unexpectedHTTP atomic.Int32
	var wsCaptureMu sync.Mutex
	var wsCapturedBytes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) && r.Header.Get("X-Rush-Test-Reference") == "fantasy-http-oracle" {
			referenceHTTP.Add(1)
		}
		if websocket.IsWebSocketUpgrade(r) {
			upgrades.Add(1)
			conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			for {
				_, frame, readErr := conn.ReadMessage()
				if readErr != nil {
					return
				}
				wsRequests.Add(1)
				wsCaptureMu.Lock()
				wsCapturedBytes += len(frame)
				if wsCapturedBytes > 1<<20 {
					wsCaptureMu.Unlock()
					t.Error("WebSocket capture exceeded 1 MiB")
					return
				}
				wsCaptureMu.Unlock()
				select {
				case wsFrames <- frame:
				default:
					t.Error("unexpected extra WebSocket request frame")
					return
				}
				for _, event := range events {
					if writeErr := conn.WriteMessage(websocket.TextMessage, []byte(event)); writeErr != nil {
						return
					}
				}
			}
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
		if err != nil {
			t.Error(err)
			http.Error(w, "could not read request", http.StatusBadRequest)
			return
		}
		if len(body) > 1<<20 {
			t.Error("reference HTTP capture exceeded 1 MiB")
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		if r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected local HTTP path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected local HTTP method: %s", r.Method)
		}
		if r.Header.Get("X-Rush-Test-Reference") != "fantasy-http-oracle" {
			unexpectedHTTP.Add(1)
			http.Error(w, "unexpected candidate HTTP fallback", http.StatusBadGateway)
			return
		}
		select {
		case referenceBodies <- body:
		default:
			t.Error("unexpected extra reference HTTP request")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			if _, err = fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.WithValue(publicResponsesTestContext(t), cliprovider.SessionIDContextKey, "public-history-composition-session"), 5*time.Second)
	defer cancel()
	network := nettransport.ResolveNetworkConfig(nil, &config.NetworkConfig{Proxy: config.ProxyNone})
	model := publicResponsesTestModel(t, server, func(context.Context) (publicResponsesPolicy, error) {
		return publicResponsesPolicy{
			apiKey:  "public-test-key",
			network: network,
			client:  publicResponsesLocalClient(server),
		}, nil
	})

	instructions, cacheKey, reasoningSummaryMode := "preserve the original instructions", "public-history-composition", "auto"
	tools := []fantasy.Tool{fantasy.FunctionTool{
		Name:        "lookup",
		Description: "Look up the requested integer.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"n": map[string]any{"type": "integer"},
			},
		},
	}}
	providerOptions := fantasy.ProviderOptions{
		fantasyopenai.Name: &fantasyopenai.ResponsesProviderOptions{
			Include:          []fantasyopenai.IncludeType{fantasyopenai.IncludeReasoningEncryptedContent},
			Instructions:     &instructions,
			PromptCacheKey:   &cacheKey,
			ReasoningSummary: &reasoningSummaryMode,
		},
	}
	firstCall := fantasy.Call{
		Prompt:          fantasy.Prompt{fantasy.NewUserMessage("original question")},
		Tools:           tools,
		ProviderOptions: providerOptions,
	}
	firstParts, err := collectPublicResponsesStream(ctx, model, firstCall)
	require.NoError(t, err)
	require.NotEmpty(t, firstParts)
	require.Equal(t, fantasy.StreamPartTypeFinish, firstParts[len(firstParts)-1].Type)

	var (
		messageID       string
		assistantTextSB strings.Builder
		reasoningMeta   *fantasyopenai.ResponsesReasoningMetadata
		toolCall        *fantasy.ToolCallPart
	)
	for _, part := range firstParts {
		switch part.Type {
		case fantasy.StreamPartTypeTextStart:
			messageID = part.ID
		case fantasy.StreamPartTypeTextDelta:
			assistantTextSB.WriteString(part.Delta)
		case fantasy.StreamPartTypeReasoningStart, fantasy.StreamPartTypeReasoningDelta, fantasy.StreamPartTypeReasoningEnd:
			metadata, ok := part.ProviderMetadata[fantasyopenai.Name].(*fantasyopenai.ResponsesReasoningMetadata)
			require.True(t, ok, "Fantasy parser must expose Responses reasoning metadata")
			reasoningMeta = metadata
		case fantasy.StreamPartTypeToolCall:
			toolCall = &fantasy.ToolCallPart{
				ToolCallID:       part.ID,
				ToolName:         part.ToolCallName,
				Input:            part.ToolCallInput,
				ProviderExecuted: part.ProviderExecuted,
			}
		}
	}
	priorText := assistantTextSB.String()
	require.Equal(t, messageItemID, messageID)
	require.Equal(t, assistantText, priorText)
	require.NotNil(t, reasoningMeta)
	require.Equal(t, reasoningItemID, reasoningMeta.ItemID)
	require.Equal(t, []string{reasoningSummary}, reasoningMeta.Summary)
	require.NotNil(t, reasoningMeta.EncryptedContent)
	require.Equal(t, encryptedReasoning, *reasoningMeta.EncryptedContent)
	require.NotNil(t, toolCall)
	require.Equal(t, functionCallID, toolCall.ToolCallID)
	require.Equal(t, "lookup", toolCall.ToolName)
	require.Equal(t, argumentText, toolCall.Input)

	priorAssistant := fantasy.Message{
		Role: fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: priorText},
			fantasy.ReasoningPart{
				Text:            strings.Join(reasoningMeta.Summary, "\n"),
				ProviderOptions: fantasy.ProviderOptions{fantasyopenai.Name: reasoningMeta},
			},
			*toolCall,
		},
	}
	retainedReasoning, ok := priorAssistant.Content[1].(fantasy.ReasoningPart)
	require.True(t, ok)
	retainedMetadata, ok := retainedReasoning.ProviderOptions[fantasyopenai.Name].(*fantasyopenai.ResponsesReasoningMetadata)
	require.True(t, ok)
	require.Equal(t, reasoningItemID, retainedMetadata.ItemID)
	require.Equal(t, []string{reasoningSummary}, retainedMetadata.Summary)
	require.NotNil(t, retainedMetadata.EncryptedContent)
	require.Equal(t, encryptedReasoning, *retainedMetadata.EncryptedContent)

	followupCall := firstCall
	followupCall.Prompt = fantasy.Prompt{
		fantasy.NewUserMessage("original question"),
		priorAssistant,
		{
			Role: fantasy.MessageRoleTool,
			Content: []fantasy.MessagePart{fantasy.ToolResultPart{
				ToolCallID: functionCallID,
				Output:     fantasy.ToolResultOutputContentText{Text: "tool result for call-history-prior"},
			}},
		},
		fantasy.NewUserMessage("follow-up question"),
	}
	secondParts, err := collectPublicResponsesStream(ctx, model, followupCall)
	require.NoError(t, err)
	require.NotEmpty(t, secondParts)
	require.Equal(t, fantasy.StreamPartTypeFinish, secondParts[len(secondParts)-1].Type)
	firstFrame := publicResponsesHistoryCapture(t, ctx, wsFrames, "first WebSocket frame")
	secondFrame := publicResponsesHistoryCapture(t, ctx, wsFrames, "second WebSocket frame")
	require.NotEmpty(t, firstFrame)

	// Run the same follow-up through the real pinned OpenAI Responses provider,
	// distinguished at the local server by a marker so fallback cannot pass as oracle.
	localHTTPProvider, err := fantasyopenai.New(
		fantasyopenai.WithAPIKey("public-test-key"),
		fantasyopenai.WithUseResponsesAPI(),
		fantasyopenai.WithHTTPClient(publicResponsesLocalClient(server)),
	)
	require.NoError(t, err)
	referenceModel, err := localHTTPProvider.LanguageModel(ctx, "gpt-5.4")
	require.NoError(t, err)
	referenceCall := followupCall
	referenceCall.Headers = map[string]string{"X-Rush-Test-Reference": "fantasy-http-oracle"}
	referenceParts, err := collectPublicResponsesStream(ctx, referenceModel, referenceCall)
	require.NoError(t, err)
	require.NotEmpty(t, referenceParts)
	require.Equal(t, fantasy.StreamPartTypeFinish, referenceParts[len(referenceParts)-1].Type)
	referenceBody := publicResponsesHistoryCapture(t, ctx, referenceBodies, "reference Fantasy HTTP request")
	expectedFullFrame, err := nettransport.ResponsesWSFullFrame(referenceBody)
	require.NoError(t, err)

	firstDecoded := publicResponsesHistoryDecode(t, firstFrame)
	actualDecoded := publicResponsesHistoryDecode(t, secondFrame)
	expectedDecoded := publicResponsesHistoryDecode(t, expectedFullFrame)
	require.Equal(t, expectedDecoded, actualDecoded, "WebSocket history must equal the pinned Fantasy HTTP serializer's full frame")
	require.NotContains(t, firstDecoded, "previous_response_id")
	require.NotContains(t, actualDecoded, "previous_response_id")
	require.Equal(t, "response.create", actualDecoded["type"])
	require.Equal(t, instructions, actualDecoded["instructions"])
	require.Equal(t, cacheKey, actualDecoded["prompt_cache_key"])
	require.Equal(t, []any{"reasoning.encrypted_content"}, actualDecoded["include"])
	reasoningOptions, ok := actualDecoded["reasoning"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "auto", reasoningOptions["summary"])
	require.Equal(t, false, actualDecoded["store"])
	firstFrameWithoutInput := make(map[string]any, len(firstDecoded)-1)
	for key, value := range firstDecoded {
		if key != "input" {
			firstFrameWithoutInput[key] = value
		}
	}
	secondFrameWithoutInput := make(map[string]any, len(actualDecoded)-1)
	for key, value := range actualDecoded {
		if key != "input" {
			secondFrameWithoutInput[key] = value
		}
	}
	require.Equal(t, firstFrameWithoutInput, secondFrameWithoutInput, "only input history should differ between WebSocket frames")
	firstInput, ok := firstDecoded["input"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, firstInput)
	require.Equal(t, "response.create", firstDecoded["type"])
	require.Equal(t, "original question", publicResponsesHistoryText(t, firstInput[0].(map[string]any), "input_text"))

	wireTools, ok := actualDecoded["tools"].([]any)
	require.True(t, ok)
	require.Len(t, wireTools, 1)
	wireTool, ok := wireTools[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "function", wireTool["type"])
	require.Equal(t, "lookup", wireTool["name"])

	input, ok := actualDecoded["input"].([]any)
	require.True(t, ok)
	// Fantasy v0.45.5 builds assistant text input items at
	// responses_language_model.go:565, function calls at :591, and intentionally
	// skips reasoning items at :596-603. Compare that replay prefix directly with
	// the actual prior response output, preserving JSON numbers canonically.
	var priorOutput []any
	priorOutputDecoder := json.NewDecoder(strings.NewReader(responseOutput))
	priorOutputDecoder.UseNumber()
	require.NoError(t, priorOutputDecoder.Decode(&priorOutput))
	require.Len(t, priorOutput, 3)
	serializedPriorOutputs := make([]any, 0, len(priorOutput)-1)
	for _, priorItem := range priorOutput {
		item, ok := priorItem.(map[string]any)
		require.True(t, ok)
		switch item["type"] {
		case "message":
			serializedPriorOutputs = append(serializedPriorOutputs, map[string]any{
				"role":    "assistant",
				"content": publicResponsesHistoryText(t, item, "output_text"),
			})
		case "reasoning":
			// The pinned serializer intentionally omits reasoning history.
		case "function_call":
			serializedPriorOutputs = append(serializedPriorOutputs, map[string]any{
				"type":      "function_call",
				"call_id":   item["call_id"],
				"name":      item["name"],
				"arguments": item["arguments"],
			})
		default:
			require.FailNow(t, "unexpected prior response output type", "%v", item["type"])
		}
	}
	require.Len(t, serializedPriorOutputs, 2, "the three prior output items replay as message and function call; reasoning is skipped")
	expectedHistoryPrefix := append([]any{firstInput[0]}, serializedPriorOutputs...)
	require.GreaterOrEqual(t, len(input), len(expectedHistoryPrefix))
	require.Equal(t,
		publicResponsesHistoryCanonical(t, expectedHistoryPrefix),
		publicResponsesHistoryCanonical(t, input[:len(expectedHistoryPrefix)]),
		"follow-up serialized input must preserve the first input and representable prior outputs",
	)
	require.Len(t, input, 5)
	assistantMessage, ok := input[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "assistant", assistantMessage["role"])
	require.NotContains(t, assistantMessage, "type", "the pinned assistant easy-input message omits the optional type field")
	require.NotContains(t, assistantMessage, "id", "Fantasy text history does not retain output message item IDs")
	require.NotContains(t, assistantMessage, "status", "Fantasy text history does not retain output message status")
	serializedToolCall, ok := input[2].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, serializedToolCall, "id", "Fantasy ToolCallPart preserves call_id but not the Responses output item ID")
	serializedToolResult, ok := input[3].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "function_call_output", serializedToolResult["type"])
	require.Equal(t, functionCallID, serializedToolResult["call_id"])
	require.Equal(t, "tool result for call-history-prior", serializedToolResult["output"])
	followupUser, ok := input[4].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "user", followupUser["role"])
	require.Equal(t, "follow-up question", publicResponsesHistoryText(t, followupUser, "input_text"))

	// The actual parsed ReasoningPart carried item ID, summary, and encrypted
	// content above. The pinned v0.45.5 serializer boundary confirms those fields
	// are not represented in replay input.
	require.NotContains(t, string(referenceBody), reasoningItemID)
	require.NotContains(t, string(referenceBody), encryptedReasoning)
	require.NotContains(t, string(referenceBody), reasoningSummary)
	require.NotContains(t, string(referenceBody), functionItemID)
	require.NotContains(t, string(expectedFullFrame), "stream")
	require.NotContains(t, string(expectedFullFrame), "background")
	require.EqualValues(t, 2, wsRequests.Load(), "exactly two inference exchanges must use WebSocket")
	require.EqualValues(t, 1, upgrades.Load(), "the accepted first exchange must reuse one WebSocket")
	require.EqualValues(t, 1, referenceHTTP.Load(), "the independent HTTP serializer oracle must be captured once")
	require.Zero(t, unexpectedHTTP.Load(), "candidate inference must not fall back to HTTP")
}

func publicResponsesHistoryText(t *testing.T, item map[string]any, expectedType string) string {
	t.Helper()
	content := item["content"]
	if text, ok := content.(string); ok {
		return text
	}
	parts, ok := content.([]any)
	require.True(t, ok)
	require.Len(t, parts, 1)
	part, ok := parts[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, expectedType, part["type"])
	text, ok := part["text"].(string)
	require.True(t, ok)
	return text
}
