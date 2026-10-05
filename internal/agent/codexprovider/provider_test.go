package codexprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type codexRoundTripper func(*http.Request) (*http.Response, error)

func (rt codexRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return rt(request)
}

func newCodexTestClient(server *httptest.Server) *http.Client {
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		panic(err)
	}
	return &http.Client{Transport: codexRoundTripper(func(request *http.Request) (*http.Response, error) {
		forwarded := request.Clone(request.Context())
		forwarded.URL.Scheme = serverURL.Scheme
		forwarded.URL.Host = serverURL.Host
		forwarded.Host = serverURL.Host
		return server.Client().Transport.RoundTrip(forwarded)
	})}
}

func codexProviderForTest(t *testing.T, server *httptest.Server) fantasy.LanguageModel {
	t.Helper()
	provider, err := New(newCodexTestClient(server), &oauth.Token{
		AccessToken: "chatgpt-oauth-access-token",
		AccountID:   "workspace-account-42",
	})
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	return model
}

func writeCodexEvent(w http.ResponseWriter, event string) {
	_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
}

func TestGenerateUsesCodexOAuthAndResponsesWire(t *testing.T) {
	type requestCapture struct {
		method  string
		path    string
		headers http.Header
		body    map[string]any
	}
	requests := make(chan requestCapture, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- requestCapture{method: r.Method, path: r.URL.Path, headers: r.Header.Clone(), body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		writeCodexEvent(w, `{"type":"response.output_text.delta","delta":"Codex reply"}`)
		writeCodexEvent(w, `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Codex reply"}]}],"usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}}`)
	}))
	t.Cleanup(server.Close)

	model := codexProviderForTest(t, server)
	choice := fantasy.ToolChoiceRequired
	ctx := context.WithValue(t.Context(), cliprovider.ReasoningEffortContextKey, "max")
	ctx = context.WithValue(ctx, cliprovider.SessionIDContextKey, "session-10")
	response, err := model.Generate(ctx, fantasy.Call{
		Prompt: fantasy.Prompt{
			{Role: fantasy.MessageRoleSystem, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "System instruction"}}},
			{Role: fantasy.MessageRole("developer"), Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Developer policy"}}},
			{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Find the answer"}}},
			{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
				fantasy.TextPart{Text: "Searching."},
				fantasy.ToolCallPart{ToolCallID: "call-previous", ToolName: "lookup", Input: `{"query":"answer"}`},
			}},
			{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
				fantasy.ToolResultPart{ToolCallID: "call-previous", Output: fantasy.ToolResultOutputContentText{Text: "Found answer"}},
			}},
		},
		Tools: []fantasy.Tool{fantasy.FunctionTool{
			Name:        "lookup",
			Description: "Search for information",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}},
		}},
		ToolChoice: &choice,
	})
	require.NoError(t, err)
	require.Equal(t, "Codex reply", response.Content.Text())
	require.Equal(t, fantasy.FinishReasonStop, response.FinishReason)
	require.Equal(t, fantasy.Usage{InputTokens: 12, OutputTokens: 3, TotalTokens: 15, CacheReadTokens: 2, ReasoningTokens: 1}, response.Usage)

	capture := <-requests
	assert.Equal(t, http.MethodPost, capture.method)
	assert.Equal(t, "/backend-api/codex/responses", capture.path)
	assert.Equal(t, "Bearer chatgpt-oauth-access-token", capture.headers.Get("Authorization"))
	assert.Equal(t, "workspace-account-42", capture.headers.Get("chatgpt-account-id"))
	assert.Equal(t, "responses=experimental", capture.headers.Get("OpenAI-Beta"))
	assert.Equal(t, "omp", capture.headers.Get("originator"))
	assert.Equal(t, codexVersion, capture.headers.Get("version"))
	assert.Equal(t, "model=gpt-5-codex", capture.headers.Get("x-codex-routing-hint"))
	assert.Equal(t, "session-10", capture.headers.Get("conversation_id"))
	assert.Equal(t, "session-10", capture.headers.Get("session_id"))
	assert.Equal(t, "session-10", capture.headers.Get("x-client-request-id"))
	assert.Empty(t, capture.headers.Get("X-Api-Key"))
	assert.Equal(t, "text/event-stream", capture.headers.Get("Accept"))
	assert.Equal(t, "gpt-5-codex", capture.body["model"])
	assert.Equal(t, true, capture.body["stream"])
	assert.Equal(t, false, capture.body["store"])
	assert.Equal(t, []any{"reasoning.encrypted_content"}, capture.body["include"])
	assert.Equal(t, map[string]any{"effort": "max", "summary": "auto"}, capture.body["reasoning"])
	assert.Equal(t, "required", capture.body["tool_choice"])
	assert.Equal(t, true, capture.body["parallel_tool_calls"])
	assert.NotContains(t, capture.body, "api_key")

	input, ok := capture.body["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 6)
	assert.Equal(t, "developer", input[0].(map[string]any)["role"])
	assert.Equal(t, "System instruction", input[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"])
	assert.Equal(t, "developer", input[1].(map[string]any)["role"])
	assert.Equal(t, "user", input[2].(map[string]any)["role"])
	assert.Equal(t, "message", input[3].(map[string]any)["type"])
	assert.Equal(t, "output_text", input[3].(map[string]any)["content"].([]any)[0].(map[string]any)["type"])
	assert.Equal(t, "function_call", input[4].(map[string]any)["type"])
	assert.Equal(t, "function_call_output", input[5].(map[string]any)["type"])
	assert.Equal(t, "Found answer", input[5].(map[string]any)["output"])
}

func TestStreamEmitsIncrementalTextAndFunctionCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeCodexEvent(w, `{"type":"response.reasoning_summary_text.delta","delta":"Plan"}`)
		writeCodexEvent(w, `{"type":"response.output_text.delta","delta":"Answer: "}`)
		writeCodexEvent(w, `{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_1","type":"function_call","call_id":"call_lookup","name":"lookup","arguments":""}}`)
		writeCodexEvent(w, `{"type":"response.function_call_arguments.delta","item_id":"fc_item_1","output_index":0,"delta":"{\"query\":"}`)
		writeCodexEvent(w, `{"type":"response.function_call_arguments.delta","item_id":"fc_item_1","output_index":0,"delta":"\"rust\"}"}`)
		writeCodexEvent(w, `{"type":"response.function_call_arguments.done","item_id":"fc_item_1","output_index":0,"arguments":"{\"query\":\"rust\"}"}`)
		writeCodexEvent(w, `{"type":"response.completed","response":{"status":"completed","output":[{"id":"fc_item_1","type":"function_call","call_id":"call_lookup","name":"lookup","arguments":"{\"query\":\"rust\"}"},{"type":"message","content":[{"type":"output_text","text":"Answer: "}]}],"usage":{"input_tokens":7,"output_tokens":5,"total_tokens":12,"output_tokens_details":{"reasoning_tokens":2}}}}`)
	}))
	t.Cleanup(server.Close)

	model := codexProviderForTest(t, server)
	stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Run a lookup"}}}}})
	require.NoError(t, err)
	var parts []fantasy.StreamPart
	for part := range stream {
		parts = append(parts, part)
	}

	var text, reasoning string
	var tool *fantasy.StreamPart
	var finish fantasy.StreamPart
	for i := range parts {
		part := parts[i]
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			text += part.Delta
		case fantasy.StreamPartTypeReasoningDelta:
			reasoning += part.Delta
		case fantasy.StreamPartTypeToolCall:
			tool = &part
		case fantasy.StreamPartTypeFinish:
			finish = part
		case fantasy.StreamPartTypeError:
			require.NoError(t, part.Error)
		}
	}
	assert.Equal(t, "Answer: ", text)
	assert.Equal(t, "Plan", reasoning)
	require.NotNil(t, tool)
	assert.Equal(t, "call_lookup", tool.ID)
	assert.Equal(t, "lookup", tool.ToolCallName)
	assert.JSONEq(t, `{"query":"rust"}`, tool.ToolCallInput)
	assert.Equal(t, fantasy.FinishReasonToolCalls, finish.FinishReason)
	assert.Equal(t, int64(12), finish.Usage.TotalTokens)
	assert.Equal(t, int64(2), finish.Usage.ReasoningTokens)
	assert.Contains(t, streamPartTypes(parts), fantasy.StreamPartTypeToolInputDelta)
	assert.Contains(t, streamPartTypes(parts), fantasy.StreamPartTypeToolInputEnd)
}

func TestGenerateReturnsCodexHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-should-retry", "true")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`)
	}))
	t.Cleanup(server.Close)
	model := codexProviderForTest(t, server)
	_, err := model.Generate(t.Context(), fantasy.Call{})
	var providerErr *fantasy.ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, http.StatusTooManyRequests, providerErr.StatusCode)
	assert.Equal(t, "rate_limit_exceeded: slow down", providerErr.Message)
	assert.True(t, providerErr.IsRetryable())
	assert.Contains(t, string(providerErr.ResponseBody), "slow down")
}

func TestStreamCancellationInterruptsResponseRead(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": waiting\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	model := codexProviderForTest(t, server)
	ctx, cancel := context.WithCancel(t.Context())
	stream, err := model.Stream(ctx, fantasy.Call{})
	require.NoError(t, err)
	parts := make(chan fantasy.StreamPart, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for part := range stream {
			if part.Type == fantasy.StreamPartTypeError {
				parts <- part
				return
			}
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Codex request did not reach fake server")
	}
	cancel()
	select {
	case part := <-parts:
		require.Error(t, part.Error)
		assert.True(t, errors.Is(part.Error, context.Canceled), "expected cancellation, got %v", part.Error)
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not stop after cancellation")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream consumer did not return")
	}
}

func streamPartTypes(parts []fantasy.StreamPart) []fantasy.StreamPartType {
	types := make([]fantasy.StreamPartType, 0, len(parts))
	for _, part := range parts {
		types = append(types, part.Type)
	}
	return types
}

func TestNewRequiresOAuthToken(t *testing.T) {
	_, err := New(nil, nil)
	require.Error(t, err)
	_, err = New(nil, &oauth.Token{AccessToken: "   "})
	require.Error(t, err)
	_, err = New(nil, &oauth.Token{AccessToken: "token"})
	require.NoError(t, err)
}

func TestSanitizeCallIDProducesCodexSafeID(t *testing.T) {
	sanitized := sanitizeCallID("call:a/1")
	assert.Regexp(t, `^call_a_1_[0-9a-f]{8}$`, sanitized)
	assert.Equal(t, sanitized, sanitizeCallID("call:a/1|fc_first"))
	assert.Equal(t, sanitized, sanitizeCallID("call:a/1\nfc_second"))
	assert.Regexp(t, `^[A-Za-z0-9_-]+$`, sanitizeCallID(""))
	assert.Equal(t, 64, len(sanitizeCallID(strings.Repeat("x", 80))))
}

// A failed Codex stream carries the backend's own code. A server-side hiccup
// (overloaded, internal error) must reach the retry classifiers as the
// retryable status it stands for: the worker turn that met
// "server_is_overloaded" used to die with no retry and the parent saw "worker
// fell without a report". Codes that are not transient keep status 0.
//
// Revert-check: return 0 from streamFailureStatus for every code and the
// retryable rows go red.
func TestStreamFailureMapsServerSideCodesToRetryableStatus(t *testing.T) {
	tests := []struct {
		name       string
		event      string
		wantStatus int
		wantRetry  bool
	}{
		{"overloaded in an error event", `{"type":"error","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded."}}`, http.StatusServiceUnavailable, true},
		{"overloaded in response.failed", `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded."}}}`, http.StatusServiceUnavailable, true},
		{"server_error", `{"type":"error","error":{"code":"server_error","message":"boom"}}`, http.StatusInternalServerError, true},
		{"rate_limit_exceeded", `{"type":"error","error":{"code":"rate_limit_exceeded","message":"slow down"}}`, http.StatusTooManyRequests, true},
		{"usage limit stays terminal", `{"type":"error","error":{"code":"usage_limit_reached","message":"The usage limit has been reached"}}`, 0, false},
		{"invalid prompt stays terminal", `{"type":"error","error":{"code":"invalid_prompt","message":"bad"}}`, 0, false},
		{"no code stays terminal", `{"type":"error","error":{"message":"boom"}}`, 0, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := newStreamState()
			err := state.consume([]byte(test.event), func(fantasy.StreamPart) bool { return true })
			var providerErr *fantasy.ProviderError
			require.ErrorAs(t, err, &providerErr)
			require.Equal(t, test.wantStatus, providerErr.StatusCode)
			require.Equal(t, test.wantRetry, providerErr.IsRetryable())
		})
	}
}
