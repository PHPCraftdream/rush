package codexprovider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type affinityCapture struct {
	method  string
	path    string
	headers http.Header
	body    map[string]any
	rawBody []byte
}

func affinityHandler(t *testing.T, captures chan<- affinityCapture, status int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read Codex request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode Codex request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		capture := affinityCapture{method: r.Method, path: r.URL.Path, headers: r.Header.Clone(), body: body, rawBody: raw}
		select {
		case captures <- capture:
		default:
			t.Errorf("Codex request capture exceeded bounded capacity")
		}
		if status != 0 {
			http.Error(w, `{"error":{"message":"rejected"}}`, status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, successfulSSE)
	}
}

func takeAffinityCapture(t *testing.T, captures <-chan affinityCapture) affinityCapture {
	t.Helper()
	select {
	case capture := <-captures:
		return capture
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Codex request capture")
		return affinityCapture{}
	}
}

func consumeAffinityStream(t *testing.T, model fantasy.LanguageModel, ctx context.Context, call fantasy.Call) []fantasy.StreamPart {
	t.Helper()
	stream, err := model.Stream(ctx, call)
	require.NoError(t, err)
	var parts []fantasy.StreamPart
	for part := range stream {
		parts = append(parts, part)
	}
	for _, part := range parts {
		if part.Type == fantasy.StreamPartTypeError {
			require.NoError(t, part.Error)
		}
	}
	return parts
}

func affinityCall(text string) fantasy.Call {
	return fantasy.Call{Prompt: fantasy.Prompt{{
		Role:    fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}},
	}}}
}

func TestCodexStreamCacheAffinityWireBodyAndOutput(t *testing.T) {
	captures := make(chan affinityCapture, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read Codex request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode Codex request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		capture := affinityCapture{method: r.Method, path: r.URL.Path, headers: r.Header.Clone(), body: body, rawBody: raw}
		select {
		case captures <- capture:
		default:
			t.Errorf("Codex request capture exceeded bounded capacity")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeCodexEvent(w, `{"type":"response.reasoning_summary_text.delta","delta":"Plan"}`)
		writeCodexEvent(w, `{"type":"response.output_text.delta","delta":"Answer: "}`)
		writeCodexEvent(w, `{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_1","type":"function_call","call_id":"call_lookup","name":"lookup","arguments":""}}`)
		writeCodexEvent(w, `{"type":"response.function_call_arguments.delta","item_id":"fc_item_1","output_index":0,"delta":"{\"query\":"}`)
		writeCodexEvent(w, `{"type":"response.function_call_arguments.delta","item_id":"fc_item_1","output_index":0,"delta":"\"rust\"}"}`)
		writeCodexEvent(w, `{"type":"response.function_call_arguments.done","item_id":"fc_item_1","output_index":0,"arguments":"{\"query\":\"rust\"}"}`)
		writeCodexEvent(w, `{"type":"response.completed","response":{"status":"completed","output":[{"id":"fc_item_1","type":"function_call","call_id":"call_lookup","name":"lookup","arguments":"{\"query\":\"rust\"}"},{"type":"message","content":[{"type":"output_text","text":"Answer: "}]}],"usage":{"input_tokens":7,"input_tokens_details":{"cached_tokens":2},"output_tokens":5,"total_tokens":12,"output_tokens_details":{"reasoning_tokens":2}}}}`)
	}))
	t.Cleanup(server.Close)
	model := codexProviderForTest(t, server)
	choice := fantasy.ToolChoiceRequired
	ctx := context.WithValue(t.Context(), cliprovider.ReasoningEffortContextKey, "max")
	ctx = context.WithValue(ctx, cliprovider.SessionIDContextKey, "wire-session")
	call := fantasy.Call{
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
	}
	stream, err := model.Stream(ctx, call)
	require.NoError(t, err)
	var parts []fantasy.StreamPart
	for part := range stream {
		parts = append(parts, part)
	}
	for _, part := range parts {
		if part.Type == fantasy.StreamPartTypeError {
			require.NoError(t, part.Error)
		}
	}

	capture := takeAffinityCapture(t, captures)
	assert.Equal(t, http.MethodPost, capture.method)
	assert.Equal(t, "/backend-api/codex/responses", capture.path)
	for header, value := range map[string]string{
		"Authorization":        "Bearer chatgpt-oauth-access-token",
		"Chatgpt-Account-Id":   "workspace-account-42",
		"Openai-Beta":          "responses=experimental",
		"Originator":           "omp",
		"Version":              codexVersion,
		"X-Codex-Routing-Hint": "model=gpt-5-codex",
		"Conversation_Id":      "wire-session",
		"Session_Id":           "wire-session",
		"X-Client-Request-Id":  "wire-session",
		"Content-Type":         "application/json",
		"Accept":               "text/event-stream",
		"User-Agent":           "rush",
	} {
		assert.Equal(t, value, capture.headers.Get(header), "request header %s", header)
	}
	assert.Empty(t, capture.headers.Get("X-Api-Key"))

	wantBody := map[string]any{
		"model": "gpt-5-codex",
		"input": []any{
			map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "System instruction"}}},
			map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "Developer policy"}}},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Find the answer"}}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Searching."}}},
			map[string]any{"type": "function_call", "call_id": "call-previous", "name": "lookup", "arguments": `{"query":"answer"}`},
			map[string]any{"type": "function_call_output", "call_id": "call-previous", "output": "Found answer"},
		},
		"stream":           true,
		"store":            false,
		"include":          []any{"reasoning.encrypted_content"},
		"prompt_cache_key": session.HashID("wire-session"),
		"reasoning":        map[string]any{"effort": "max", "summary": "auto"},
		"tools": []any{map[string]any{
			"type": "function", "name": "lookup", "description": "Search for information",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}},
		}},
		"parallel_tool_calls": true,
		"tool_choice":         "required",
	}
	assert.Equal(t, wantBody, capture.body, "complete decoded Responses request")

	var text, reasoning string
	var tool *fantasy.StreamPart
	var finish *fantasy.StreamPart
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
			finish = &part
		}
	}
	assert.Equal(t, "Answer: ", text)
	assert.Equal(t, "Plan", reasoning)
	require.NotNil(t, tool)
	assert.Equal(t, "call_lookup", tool.ID)
	assert.Equal(t, "lookup", tool.ToolCallName)
	assert.JSONEq(t, `{"query":"rust"}`, tool.ToolCallInput)
	require.NotNil(t, finish)
	assert.Equal(t, fantasy.FinishReasonToolCalls, finish.FinishReason)
	assert.Equal(t, fantasy.Usage{InputTokens: 5, OutputTokens: 5, TotalTokens: 12, CacheReadTokens: 2, ReasoningTokens: 2}, finish.Usage)
}

func TestCodexSessionCacheAffinityStableAcrossPromptAndTokenRotation(t *testing.T) {
	captures := make(chan affinityCapture, 3)
	server := httptest.NewServer(affinityHandler(t, captures, 0))
	t.Cleanup(server.Close)
	source := newTestSource()
	provider, err := NewWithSource(newCodexTestClient(server), source)
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)

	firstCtx := context.WithValue(t.Context(), cliprovider.SessionIDContextKey, "stable-session")
	firstStream, err := model.Stream(firstCtx, affinityCall("prompt before rotation"))
	require.NoError(t, err)
	var firstParts []fantasy.StreamPart
	for part := range firstStream {
		firstParts = append(firstParts, part)
	}
	for _, part := range firstParts {
		if part.Type == fantasy.StreamPartTypeError {
			require.NoError(t, part.Error)
		}
	}
	secondStream, err := model.Stream(firstCtx, affinityCall("changed prompt after rotation"))
	require.NoError(t, err)
	rotated := rotatedToken()
	source.set(rotated, true)
	var secondParts []fantasy.StreamPart
	for part := range secondStream {
		secondParts = append(secondParts, part)
	}
	for _, part := range secondParts {
		if part.Type == fantasy.StreamPartTypeError {
			require.NoError(t, part.Error)
		}
	}
	distinctCtx := context.WithValue(t.Context(), cliprovider.SessionIDContextKey, "different-session")
	consumeAffinityStream(t, model, distinctCtx, affinityCall("independent session"))

	first := takeAffinityCapture(t, captures)
	second := takeAffinityCapture(t, captures)
	third := takeAffinityCapture(t, captures)
	wantStable := session.HashID("stable-session")
	wantDistinct := session.HashID("different-session")
	assert.Equal(t, wantStable, first.body["prompt_cache_key"])
	assert.Equal(t, wantStable, second.body["prompt_cache_key"])
	assert.Equal(t, wantDistinct, third.body["prompt_cache_key"])
	assert.NotEqual(t, first.body["prompt_cache_key"], third.body["prompt_cache_key"])
	assert.Equal(t, "Bearer old-access", first.headers.Get("Authorization"), "Current is read when the stream is consumed")
	assert.Equal(t, "old-account", first.headers.Get("chatgpt-account-id"))
	assert.Equal(t, "Bearer new-access", second.headers.Get("Authorization"))
	assert.Equal(t, "new-account", second.headers.Get("chatgpt-account-id"))
	assert.Equal(t, "stable-session", first.headers.Get("session_id"))
	assert.Equal(t, "stable-session", second.headers.Get("session_id"))
	assert.Equal(t, "different-session", third.headers.Get("session_id"))
	assert.Equal(t, "prompt before rotation", first.body["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"])
	assert.Equal(t, "changed prompt after rotation", second.body["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"])
}

func TestCodexTypedPromptCacheKeyPrecedenceAndUnsupportedOptions(t *testing.T) {
	effort := openai.ReasoningEffort("high")
	summary := "auto"
	tests := []struct {
		name        string
		ctx         context.Context
		options     fantasy.ProviderOptions
		wantKey     string
		wantPresent bool
	}{
		{
			name: "typed explicit key outranks session",
			ctx:  context.WithValue(t.Context(), cliprovider.SessionIDContextKey, "session-value"),
			options: fantasy.ProviderOptions{openai.Name: &openai.ResponsesProviderOptions{
				PromptCacheKey: stringPointer("explicit-key"),
			}},
			wantKey: "explicit-key", wantPresent: true,
		},
		{
			name: "nonempty explicit key works without context",
			ctx:  t.Context(),
			options: fantasy.ProviderOptions{openai.Name: &openai.ResponsesProviderOptions{
				PromptCacheKey: stringPointer("explicit-without-context"),
			}},
			wantKey: "explicit-without-context", wantPresent: true,
		},
		{
			name: "explicit empty typed key overrides session",
			ctx:  context.WithValue(t.Context(), cliprovider.SessionIDContextKey, "session-value"),
			options: fantasy.ProviderOptions{openai.Name: &openai.ResponsesProviderOptions{
				PromptCacheKey: stringPointer(""),
			}},
			wantKey: "", wantPresent: true,
		},
		{
			name: "explicit empty typed key works without context",
			ctx:  t.Context(),
			options: fantasy.ProviderOptions{openai.Name: &openai.ResponsesProviderOptions{
				PromptCacheKey: stringPointer(""),
			}},
			wantKey: "", wantPresent: true,
		},
		{
			name:    "nil typed pointer falls back",
			ctx:     context.WithValue(t.Context(), cliprovider.SessionIDContextKey, "fallback-session"),
			options: fantasy.ProviderOptions{openai.Name: (*openai.ResponsesProviderOptions)(nil)},
			wantKey: session.HashID("fallback-session"), wantPresent: true,
		},
		{
			name:    "nil typed key falls back",
			ctx:     context.WithValue(t.Context(), cliprovider.SessionIDContextKey, "fallback-session"),
			options: fantasy.ProviderOptions{openai.Name: &openai.ResponsesProviderOptions{}},
			wantKey: session.HashID("fallback-session"), wantPresent: true,
		},
		{
			name: "non-cache Responses options are ignored",
			ctx:  t.Context(),
			options: fantasy.ProviderOptions{openai.Name: &openai.ResponsesProviderOptions{
				ReasoningEffort:  &effort,
				ReasoningSummary: &summary,
			}},
		},
		{
			name: "unsupported typed provider options are ignored",
			ctx:  t.Context(),
			options: fantasy.ProviderOptions{
				openai.Name:          &openai.ProviderOptions{PromptCacheKey: stringPointer("ignored-chat-key")},
				"untrusted-provider": &openai.ResponsesProviderOptions{PromptCacheKey: stringPointer("ignored-other-key")},
			},
		},
		{
			name: "no session or options omits key",
			ctx:  t.Context(),
		},
		{
			name: "empty session omits key",
			ctx:  context.WithValue(t.Context(), cliprovider.SessionIDContextKey, ""),
		},
		{
			name: "wrong type session omits key",
			ctx:  context.WithValue(t.Context(), cliprovider.SessionIDContextKey, 42),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			captures := make(chan affinityCapture, 1)
			server := httptest.NewServer(affinityHandler(t, captures, 0))
			t.Cleanup(server.Close)
			model := codexProviderForTest(t, server)
			call := affinityCall("option boundary")
			call.ProviderOptions = test.options
			consumeAffinityStream(t, model, test.ctx, call)
			capture := takeAffinityCapture(t, captures)
			assert.Equal(t, http.MethodPost, capture.method)
			assert.Equal(t, "/backend-api/codex/responses", capture.path)
			actual, present := capture.body["prompt_cache_key"]
			assert.Equal(t, test.wantPresent, present)
			if present {
				assert.Equal(t, test.wantKey, actual)
			}
			wantKeys := []string{"model", "input", "stream", "store", "include"}
			if test.wantPresent {
				wantKeys = append(wantKeys, "prompt_cache_key")
			}
			assert.ElementsMatch(t, wantKeys, bodyKeys(capture.body), "no provider options or extra body fields are forwarded")
		})
	}
}

func TestCodexCacheAffinitySurvivesRefreshRetry(t *testing.T) {
	captures := make(chan affinityCapture, 2)
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := sends.Add(1)
		status := 0
		if n == 1 {
			status = http.StatusUnauthorized
		}
		affinityHandler(t, captures, status)(w, r)
	}))
	t.Cleanup(server.Close)
	source := newTestSource()
	source.refreshHook = func(_ context.Context, _ oauth.Token) error {
		source.set(rotatedToken(), true)
		return nil
	}
	provider, err := NewWithSource(newCodexTestClient(server), source)
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), cliprovider.SessionIDContextKey, "retry-session")
	consumeAffinityStream(t, model, ctx, affinityCall("same retry body"))

	first := takeAffinityCapture(t, captures)
	second := takeAffinityCapture(t, captures)
	assert.Equal(t, first.rawBody, second.rawBody, "401 retry must replay byte-identical request body")
	assert.Equal(t, session.HashID("retry-session"), second.body["prompt_cache_key"])
	assert.Equal(t, "Bearer old-access", first.headers.Get("Authorization"))
	assert.Equal(t, "old-account", first.headers.Get("chatgpt-account-id"))
	assert.Equal(t, "Bearer new-access", second.headers.Get("Authorization"))
	assert.Equal(t, "new-account", second.headers.Get("chatgpt-account-id"))
	assert.EqualValues(t, 2, sends.Load())
	assert.EqualValues(t, 1, source.refreshes.Load())
}

func TestCodexLogoutBeforeStreamConsumptionSendsNothing(t *testing.T) {
	captures := make(chan affinityCapture, 1)
	server := httptest.NewServer(affinityHandler(t, captures, 0))
	t.Cleanup(server.Close)
	source := newTestSource()
	provider, err := NewWithSource(newCodexTestClient(server), source)
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), cliprovider.SessionIDContextKey, "logout-session")
	stream, err := model.Stream(ctx, affinityCall("logged out"))
	require.NoError(t, err)
	source.set(oauth.Token{}, false)
	var streamErr error
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			streamErr = part.Error
		}
	}
	require.Error(t, streamErr)
	assert.Contains(t, streamErr.Error(), "credentials unavailable")
	select {
	case capture := <-captures:
		t.Fatalf("logout unexpectedly sent a request: %#v", capture)
	default:
	}
}

func stringPointer(value string) *string { return &value }

func bodyKeys(body map[string]any) []string {
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	return keys
}
