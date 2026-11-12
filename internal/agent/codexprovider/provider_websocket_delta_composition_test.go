package codexprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/nettransport"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type codexDeltaCompositionPeer struct {
	frames       chan []byte
	upgrades     atomic.Int32
	requests     atomic.Int32
	httpRequests atomic.Int32
}

func newCodexDeltaCompositionPeer(t *testing.T, completions ...string) (*httptest.Server, *codexDeltaCompositionPeer) {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(key, "")
	}
	peer := &codexDeltaCompositionPeer{frames: make(chan []byte, len(completions))}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			peer.httpRequests.Add(1)
			http.Error(w, "unexpected HTTP fallback", http.StatusInternalServerError)
			return
		}
		peer.upgrades.Add(1)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade Codex WebSocket: %v", err)
			return
		}
		defer conn.Close()
		for _, completion := range completions {
			_, frame, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			peer.frames <- append([]byte(nil), frame...)
			peer.requests.Add(1)
			if writeErr := conn.WriteMessage(websocket.TextMessage, []byte(completion)); writeErr != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server, peer
}

func codexDeltaReceiveFrame(t *testing.T, ctxDone <-chan struct{}, frames <-chan []byte) []byte {
	t.Helper()
	select {
	case frame := <-frames:
		return frame
	case <-ctxDone:
		t.Fatal("timed out waiting for captured Codex WebSocket frame")
		return nil
	}
}

func codexDeltaAssistantParts(t *testing.T, response *fantasy.Response) []fantasy.MessagePart {
	t.Helper()
	parts := make([]fantasy.MessagePart, 0, len(response.Content))
	for _, content := range response.Content {
		switch part := content.(type) {
		case fantasy.TextContent:
			parts = append(parts, fantasy.TextPart{Text: part.Text})
		case fantasy.ReasoningContent:
			parts = append(parts, fantasy.ReasoningPart{Text: part.Text})
		case fantasy.ToolCallContent:
			parts = append(parts, fantasy.ToolCallPart{ToolCallID: part.ToolCallID, ToolName: part.ToolName, Input: part.Input})
		default:
			t.Fatalf("unexpected parsed Codex response content %T", content)
		}
	}
	return parts
}

func codexDeltaExchange(t *testing.T, firstCompletion, question string, compose func(*testing.T, *fantasy.Response) fantasy.Prompt) (*fantasy.Response, fantasy.Call, []byte) {
	t.Helper()
	server, peer := newCodexDeltaCompositionPeer(t, firstCompletion, `{"type":"response.completed","response":{"id":"resp-followup","status":"completed","output":[]}}`)
	ctx, cancel := context.WithTimeout(websocketCodexContext(t), 5*time.Second)
	defer cancel()
	model := websocketCodexModel(t, server, newTestSource())

	tools := []fantasy.Tool{fantasy.FunctionTool{
		Name:        "lookup",
		Description: "Look up a value",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"count": map[string]any{"type": "integer"}}},
	}}
	choice := fantasy.ToolChoiceRequired
	options := func(prompt fantasy.Prompt) fantasy.Call {
		return fantasy.Call{Prompt: prompt, Tools: tools, ToolChoice: &choice}
	}
	first, err := model.Generate(ctx, options(fantasy.Prompt{fantasy.NewUserMessage(question)}))
	require.NoError(t, err)
	nextCall := options(compose(t, first))
	_, err = model.Generate(ctx, nextCall)
	require.NoError(t, err)

	firstFrame := codexDeltaReceiveFrame(t, ctx.Done(), peer.frames)
	secondFrame := codexDeltaReceiveFrame(t, ctx.Done(), peer.frames)
	firstFields := codexDeltaFrameJSONValue(t, firstFrame)
	secondFields := codexDeltaFrameJSONValue(t, secondFrame)
	require.NotContains(t, firstFields, "previous_response_id")
	require.NotContains(t, secondFields, "previous_response_id")
	require.Equal(t, true, firstFields["stream"])
	require.Equal(t, true, secondFields["stream"])
	delete(firstFields, "input")
	delete(secondFields, "input")
	require.True(t, reflect.DeepEqual(firstFields, secondFields), "first and second frames must preserve identical options and tools")
	body, err := makeRequestBody(ctx, model.Model(), nextCall)
	require.NoError(t, err)
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	wantFrame, err := nettransport.ResponsesWSCodexFullFrame(payload)
	require.NoError(t, err)
	requireCodexDeltaJSONEqual(t, wantFrame, secondFrame, "second request must be the full native Codex frame")
	require.NotEmpty(t, firstFrame)
	require.EqualValues(t, 2, peer.requests.Load(), "two sequential inference frames")
	require.EqualValues(t, 1, peer.upgrades.Load(), "accepted completion should reuse the native WebSocket")
	require.Zero(t, peer.httpRequests.Load(), "sequential inference must not fall back to HTTP")
	return first, nextCall, secondFrame
}

// UseNumber retains numeric tokens (including values above 2^53) while map
// decoding makes this full-frame oracle independent of JSON object key order.
func requireCodexDeltaJSONEqual(t *testing.T, want, got []byte, message string) {
	t.Helper()
	require.True(t, reflect.DeepEqual(codexDeltaJSONValue(t, want), codexDeltaJSONValue(t, got)), message)
}

func codexDeltaJSONValue(t *testing.T, raw []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	require.NoError(t, decoder.Decode(&value))
	return value
}

func codexDeltaFrameJSONValue(t *testing.T, frame []byte) map[string]any {
	t.Helper()
	var rawFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(frame, &rawFields))
	fields := make(map[string]any, len(rawFields))
	for name, raw := range rawFields {
		fields[name] = codexDeltaJSONValue(t, raw)
	}
	return fields
}

func codexDeltaInput(t *testing.T, frame []byte) []json.RawMessage {
	t.Helper()
	var request struct {
		Type               string            `json:"type"`
		Input              []json.RawMessage `json:"input"`
		PreviousResponseID json.RawMessage   `json:"previous_response_id"`
	}
	require.NoError(t, json.Unmarshal(frame, &request))
	require.Equal(t, "response.create", request.Type)
	require.Empty(t, request.PreviousResponseID, "native history is not server-stored chaining")
	return request.Input
}

func codexDeltaOutputItems(t *testing.T, completion string) []json.RawMessage {
	t.Helper()
	var event struct {
		Response struct {
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	require.NoError(t, json.Unmarshal([]byte(completion), &event))
	return event.Response.Output
}

func TestCodexWebSocketDeltaCompositionKeepsFullTextHistory(t *testing.T) {
	completion := `{"type":"response.completed","response":{"id":"resp-text","status":"completed","output":[{"id":"msg-text","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"visible answer","annotations":[{"type":"url_citation","url":"https://example.test/source","title":"source","start_index":0,"end_index":5}]}]}]}}`
	first, _, secondFrame := codexDeltaExchange(t, completion, "text question", func(t *testing.T, response *fantasy.Response) fantasy.Prompt {
		parts := codexDeltaAssistantParts(t, response)
		require.Equal(t, []fantasy.MessagePart{fantasy.TextPart{Text: "visible answer"}}, parts)
		return fantasy.Prompt{
			fantasy.NewUserMessage("text question"),
			{Role: fantasy.MessageRoleAssistant, Content: parts},
			fantasy.NewUserMessage("follow-up"),
		}
	})
	require.Equal(t, "visible answer", first.Content.Text())
	outputs := codexDeltaOutputItems(t, completion)
	require.Len(t, outputs, 1)
	require.JSONEq(t, `{"id":"msg-text","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"visible answer","annotations":[{"type":"url_citation","url":"https://example.test/source","title":"source","start_index":0,"end_index":5}]}]}`, string(outputs[0]))

	input := codexDeltaInput(t, secondFrame)
	require.Len(t, input, 3)
	require.JSONEq(t, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"visible answer"}]}`, string(input[1]))
	require.NotEqual(t, codexDeltaJSONValue(t, outputs[0]), codexDeltaJSONValue(t, input[1]), "native completed output is not the serialized Fantasy assistant history")
	var represented map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(input[1], &represented))
	require.NotContains(t, represented, "id")
	require.NotContains(t, represented, "status")
	var content []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(represented["content"], &content))
	require.NotContains(t, content[0], "annotations", "the parser/Fantasy/serializer history has no citation annotation field")
}

func TestCodexWebSocketDeltaCompositionKeepsToolArgumentsAndResult(t *testing.T) {
	const arguments = `{ "count" : 9007199254740993, "label" : "exact whitespace" }`
	completion := `{"type":"response.completed","response":{"id":"resp-tool","status":"completed","output":[{"id":"fc-item","type":"function_call","status":"completed","call_id":"call-precise","name":"lookup","arguments":"{ \"count\" : 9007199254740993, \"label\" : \"exact whitespace\" }"}]}}`
	first, _, secondFrame := codexDeltaExchange(t, completion, "tool question", func(t *testing.T, response *fantasy.Response) fantasy.Prompt {
		parts := codexDeltaAssistantParts(t, response)
		require.Len(t, parts, 1)
		call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](parts[0])
		require.True(t, ok)
		require.Equal(t, "call-precise", call.ToolCallID)
		require.Equal(t, "lookup", call.ToolName)
		require.Equal(t, arguments, call.Input)
		return fantasy.Prompt{
			fantasy.NewUserMessage("tool question"),
			{Role: fantasy.MessageRoleAssistant, Content: parts},
			{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{ToolCallID: call.ToolCallID, Output: fantasy.ToolResultOutputContentText{Text: "exact tool result"}},
			}},
			fantasy.NewUserMessage("follow-up"),
		}
	})
	require.Equal(t, arguments, first.Content.ToolCalls()[0].Input)
	outputs := codexDeltaOutputItems(t, completion)
	require.Len(t, outputs, 1)
	require.JSONEq(t, `{"id":"fc-item","type":"function_call","status":"completed","call_id":"call-precise","name":"lookup","arguments":"{ \"count\" : 9007199254740993, \"label\" : \"exact whitespace\" }"}`, string(outputs[0]))

	input := codexDeltaInput(t, secondFrame)
	require.Len(t, input, 4)
	require.JSONEq(t, `{"type":"function_call","call_id":"call-precise","name":"lookup","arguments":"{ \"count\" : 9007199254740993, \"label\" : \"exact whitespace\" }"}`, string(input[1]))
	require.NotEqual(t, codexDeltaJSONValue(t, outputs[0]), codexDeltaJSONValue(t, input[1]), "native completed output is not the serialized Fantasy assistant history")
	require.JSONEq(t, `{"type":"function_call_output","call_id":"call-precise","output":"exact tool result"}`, string(input[2]))
	var represented map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(input[1], &represented))
	require.NotContains(t, represented, "id", "Fantasy function-call history does not represent the output item ID")
	require.NotContains(t, represented, "status", "Fantasy function-call history does not represent the output status")
}

func TestCodexWebSocketDeltaCompositionMarksEncryptedReasoningBoundary(t *testing.T) {
	completion := `{"type":"response.completed","response":{"id":"resp-reasoning","status":"completed","output":[{"id":"rs-encrypted","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"parsed summary"}],"encrypted_content":"opaque-native-reasoning"}]}}`
	first, _, secondFrame := codexDeltaExchange(t, completion, "reasoning question", func(t *testing.T, response *fantasy.Response) fantasy.Prompt {
		parts := codexDeltaAssistantParts(t, response)
		require.Equal(t, []fantasy.MessagePart{fantasy.ReasoningPart{Text: "parsed summary"}}, parts)
		return fantasy.Prompt{
			fantasy.NewUserMessage("reasoning question"),
			{Role: fantasy.MessageRoleAssistant, Content: parts},
			fantasy.NewUserMessage("follow-up"),
		}
	})
	require.Equal(t, "parsed summary", first.Content.ReasoningText())
	outputs := codexDeltaOutputItems(t, completion)
	require.Len(t, outputs, 1)
	require.JSONEq(t, `{"id":"rs-encrypted","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"parsed summary"}],"encrypted_content":"opaque-native-reasoning"}`, string(outputs[0]))
	var actualOutput map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(outputs[0], &actualOutput))
	require.JSONEq(t, `"rs-encrypted"`, string(actualOutput["id"]))
	require.JSONEq(t, `"opaque-native-reasoning"`, string(actualOutput["encrypted_content"]))

	input := codexDeltaInput(t, secondFrame)
	// The actual parsed Fantasy history contains a ReasoningPart, but
	// makeRequestBody intentionally omits reasoning history, including its
	// summary and the output's encrypted_content.
	require.Len(t, input, 2)
	require.JSONEq(t, `{"type":"message","role":"user","content":[{"type":"input_text","text":"reasoning question"}]}`, string(input[0]))
	require.JSONEq(t, `{"type":"message","role":"user","content":[{"type":"input_text","text":"follow-up"}]}`, string(input[1]))
	require.NotContains(t, string(secondFrame), "opaque-native-reasoning")
	require.NotContains(t, string(secondFrame), "parsed summary")
}
