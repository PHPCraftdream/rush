package agent

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
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/nettransport"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type publicResponsesTestTransport func(*http.Request) (*http.Response, error)

func (f publicResponsesTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func publicResponsesLocalClient(server *httptest.Server) *http.Client {
	target, _ := url.Parse(server.URL)
	return &http.Client{Transport: publicResponsesTestTransport(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		copy.URL.Scheme, copy.URL.Host = target.Scheme, target.Host
		copy.Host = target.Host
		return server.Client().Transport.RoundTrip(copy)
	})}
}

func publicResponsesTestModel(t *testing.T, server *httptest.Server, current func(context.Context) (publicResponsesPolicy, error), owners ...*coordinator) fantasy.LanguageModel {
	t.Helper()
	manager := nettransport.NewResponsesWSManager(t.Context(), 4)
	if len(owners) > 0 {
		owners[0].responsesWS = manager
	}
	t.Cleanup(func() { _ = manager.Close() })
	bridge := &publicResponsesClient{manager: manager, current: current, endpoint: strings.Replace(server.URL, "http://", "ws://", 1)}
	p, err := newPublicResponsesSDKProvider("https://api.openai.com/v1", "public-test-key", nil, bridge)
	require.NoError(t, err)
	wrapped := &publicResponsesProvider{Provider: p}
	model, err := wrapped.LanguageModel(t.Context(), "gpt-5.4")
	require.NoError(t, err)
	return model
}

func publicResponsesTestContext(t *testing.T) context.Context {
	return context.WithValue(t.Context(), cliprovider.SessionIDContextKey, "public-session")
}

func collectPublicResponsesStream(ctx context.Context, model fantasy.LanguageModel, call fantasy.Call) ([]fantasy.StreamPart, error) {
	stream, err := model.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	var parts []fantasy.StreamPart
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			return parts, part.Error
		}
		parts = append(parts, part)
	}
	return parts, nil
}

var publicResponsesTestEvents = []string{
	`{"type":"response.created","response":{"id":"resp-public","status":"in_progress"}}`,
	`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg-public","role":"assistant","content":[]}}`,
	`{"type":"response.output_text.delta","item_id":"msg-public","output_index":0,"content_index":0,"delta":"answer"}`,
	`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg-public","role":"assistant","content":[{"type":"output_text","text":"answer"}]}}`,
	`{"type":"response.output_item.added","output_index":1,"item":{"type":"reasoning","id":"rs-public","summary":[],"encrypted_content":"opaque"}}`,
	`{"type":"response.reasoning_summary_part.added","item_id":"rs-public","output_index":1,"summary_index":0,"part":{"type":"summary_text","text":""}}`,
	`{"type":"response.reasoning_summary_text.delta","item_id":"rs-public","output_index":1,"summary_index":0,"delta":"reason"}`,
	`{"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","id":"rs-public","summary":[{"type":"summary_text","text":"reason"}],"encrypted_content":"opaque"}}`,
	`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc-public","call_id":"call-public","name":"lookup","arguments":""}}`,
	`{"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc-public","delta":"{ \"n\": 9007199254740993 }"}`,
	`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc-public","call_id":"call-public","name":"lookup","arguments":"{ \"n\": 9007199254740993 }"}}`,
	`{"type":"response.completed","response":{"id":"resp-public","status":"completed","output":[],"usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}}`,
}

func TestPublicResponsesWebSocketPinnedSDKParityAndReuse(t *testing.T) {
	var upgrades, requests, httpRequests atomic.Int32
	frames, httpFrames, headers := make(chan []byte, 8), make(chan []byte, 4), make(chan http.Header, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			httpRequests.Add(1)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			httpFrames <- body
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range publicResponsesTestEvents {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
			}
			return
		}
		upgrades.Add(1)
		headers <- r.Header.Clone()
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			requests.Add(1)
			frames <- raw
			for _, event := range publicResponsesTestEvents {
				if err = conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
					return
				}
			}
		}
	}))
	defer server.Close()
	client := publicResponsesLocalClient(server)
	policy := publicResponsesPolicy{apiKey: "public-test-key", headers: map[string]string{"OpenAI-Project": "project-test"}, client: client, network: config.NetworkConfig{Proxy: config.ProxyNone}}
	model := publicResponsesTestModel(t, server, func(context.Context) (publicResponsesPolicy, error) { return policy, nil })
	p, err := openai.New(openai.WithAPIKey("public-test-key"), openai.WithUseResponsesAPI(), openai.WithHTTPClient(client))
	require.NoError(t, err)
	referenceModel, err := p.LanguageModel(t.Context(), "gpt-5.4")
	require.NoError(t, err)
	cacheKey, instructions := "cache-test", "precise"
	call := fantasy.Call{
		Prompt:          fantasy.Prompt{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "question"}}}},
		Headers:         map[string]string{"OpenAI-Project": "call-project"},
		ProviderOptions: fantasy.ProviderOptions{openai.Name: &openai.ResponsesProviderOptions{PromptCacheKey: &cacheKey, Instructions: &instructions}},
		Tools:           []fantasy.Tool{fantasy.FunctionTool{Name: "lookup", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "integer"}}}}},
	}
	reference, err := collectPublicResponsesStream(publicResponsesTestContext(t), referenceModel, call)
	require.NoError(t, err)
	seenTool, seenReason := false, false
	for _, part := range reference {
		if part.Type == fantasy.StreamPartTypeToolCall {
			seenTool = true
			require.Equal(t, `{ "n": 9007199254740993 }`, part.ToolCallInput)
		}
		if part.Type == fantasy.StreamPartTypeReasoningDelta && part.Delta == "reason" {
			seenReason = true
		}
	}
	require.True(t, seenTool)
	require.True(t, seenReason)
	require.EqualValues(t, 2, reference[len(reference)-1].Usage.CacheReadTokens)
	expected, err := nettransport.ResponsesWSFullFrame(<-httpFrames)
	require.NoError(t, err)
	for range 2 {
		parts, err := collectPublicResponsesStream(publicResponsesTestContext(t), model, call)
		require.NoError(t, err)
		require.Equal(t, reference, parts)
		raw := <-frames
		require.JSONEq(t, string(expected), string(raw))
		var body map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &body))
		require.NotContains(t, body, "stream")
		require.NotContains(t, body, "background")
		require.NotContains(t, body, "previous_response_id")
		require.JSONEq(t, `"response.create"`, string(body["type"]))
	}
	header := <-headers
	require.Equal(t, "Bearer public-test-key", header.Get("Authorization"))
	require.Equal(t, "call-project", header.Get("OpenAI-Project"))
	require.Empty(t, header.Get("chatgpt-account-id"))
	require.Empty(t, header.Get("OpenAI-Beta"))
	// Full body parity includes pinned SDK options/tools rather than a rewrite.
	var expectedBody map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(expected, &expectedBody))
	require.JSONEq(t, `false`, string(expectedBody["store"]))
	require.EqualValues(t, 1, upgrades.Load())
	require.EqualValues(t, 2, requests.Load())
	require.EqualValues(t, 1, httpRequests.Load())
}

func TestPublicResponsesWebSocketAmbiguousSendBlocksFantasyAndCoordinatorRetries(t *testing.T) {
	var requests, httpRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			httpRequests.Add(1)
			w.WriteHeader(500)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if _, _, err = conn.ReadMessage(); err == nil {
			requests.Add(1)
		}
	}))
	defer server.Close()
	model := publicResponsesTestModel(t, server, func(context.Context) (publicResponsesPolicy, error) {
		return publicResponsesPolicy{apiKey: "public-test-key", client: publicResponsesLocalClient(server)}, nil
	})
	retry := fantasy.RetryWithExponentialBackoffRespectingRetryHeaders[[]fantasy.StreamPart](fantasy.RetryOptions{MaxRetries: 3, InitialDelayIn: time.Nanosecond})
	_, err := retry(publicResponsesTestContext(t), func() ([]fantasy.StreamPart, error) {
		return collectPublicResponsesStream(publicResponsesTestContext(t), model, fantasy.Call{})
	})
	require.Error(t, err)
	require.True(t, responsesWSReplayForbidden(err))
	require.Equal(t, classTerminal, classifyProviderError(err))
	joined := errors.Join(&fantasy.ProviderError{StatusCode: http.StatusUnauthorized}, err)
	require.False(t, (&coordinator{}).isUnauthorized(joined))
	require.EqualValues(t, 1, requests.Load())
	require.Zero(t, httpRequests.Load())
	// A watchdog's persisted title must not override the send-state fence.
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial_%v", partial), func(t *testing.T) {
			parts := []message.ContentPart{message.Finish{Reason: message.FinishReasonError, Message: streamStalledFinishTitle}}
			if partial {
				parts = append([]message.ContentPart{message.TextContent{Text: "partial"}}, parts...)
			}
			coord, sid, id := appendAssistant(t, testEnv(t), parts)
			require.False(t, coord.shouldRetryTurn(t.Context(), sid, err, id))
			_, resume := coord.shouldContinueTurn(t.Context(), sid, err, id)
			require.False(t, resume)
		})
	}
}

func TestPublicResponsesWebSocketUnsupportedFallbackBeforeSendOnly(t *testing.T) {
	var upgrades, httpRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			upgrades.Add(1)
			w.WriteHeader(http.StatusUpgradeRequired)
			return
		}
		httpRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range publicResponsesTestEvents {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	defer server.Close()
	model := publicResponsesTestModel(t, server, func(context.Context) (publicResponsesPolicy, error) {
		return publicResponsesPolicy{apiKey: "public-test-key", client: publicResponsesLocalClient(server)}, nil
	})
	for range 2 {
		_, err := collectPublicResponsesStream(publicResponsesTestContext(t), model, fantasy.Call{})
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, upgrades.Load())
	require.EqualValues(t, 2, httpRequests.Load())
}

type responsesWSCancelAgent struct {
	*mockSessionAgent
	cancel context.CancelFunc
}

func (a *responsesWSCancelAgent) Cancel(string) { a.cancel() }

type responsesWSCancelFacts struct{ unknownBatchCutoverFacts }

func (responsesWSCancelFacts) LiveDelegationNodes(context.Context, string) ([]session.BatchLiveNode, error) {
	return nil, nil
}

func TestPublicResponsesWebSocketCancellationAllowsLaterRequest(t *testing.T) {
	var requests, httpRequests atomic.Int32
	entered, closed := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			httpRequests.Add(1)
			w.WriteHeader(500)
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if _, _, err = conn.ReadMessage(); err != nil {
			return
		}
		if requests.Add(1) == 1 {
			close(entered)
			_, _, _ = conn.ReadMessage()
			close(closed)
			return
		}
		for _, event := range publicResponsesTestEvents {
			if err = conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(publicResponsesTestContext(t))
	defer cancel()
	coord := &coordinator{currentAgent: &responsesWSCancelAgent{mockSessionAgent: &mockSessionAgent{}, cancel: cancel}, subAgentDrivers: newSubAgentDriverRegistry()}
	BindCoordinatorBatchCutoverFacts(coord, responsesWSCancelFacts{})
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(coord) })
	model := publicResponsesTestModel(t, server, func(context.Context) (publicResponsesPolicy, error) {
		return publicResponsesPolicy{apiKey: "public-test-key", client: publicResponsesLocalClient(server)}, nil
	}, coord)
	done := make(chan error, 1)
	go func() { _, err := collectPublicResponsesStream(ctx, model, fantasy.Call{}); done <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach peer")
	}
	coord.Cancel("public-session")
	require.False(t, coord.responsesWS.Closed())
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
		require.True(t, responsesWSReplayForbidden(err))
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not unwind exchange")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not close peer")
	}
	_, err := collectPublicResponsesStream(publicResponsesTestContext(t), model, fantasy.Call{})
	require.NoError(t, err)
	require.EqualValues(t, 2, requests.Load())
	coord.CancelAll()
	require.True(t, coord.responsesWS.Closed())
	_, err = collectPublicResponsesStream(publicResponsesTestContext(t), model, fantasy.Call{})
	require.Error(t, err)
	require.EqualValues(t, 2, requests.Load())
	require.Zero(t, httpRequests.Load())
}

func TestPublicResponsesWebSocketCurrentCredentialsNetworkAndLogout(t *testing.T) {
	var requests, httpRequests atomic.Int32
	headers, closed := make(chan http.Header, 8), make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			httpRequests.Add(1)
			w.WriteHeader(500)
			return
		}
		headers <- r.Header.Clone()
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close(); closed <- struct{}{} }()
		for {
			if _, _, err = conn.ReadMessage(); err != nil {
				return
			}
			requests.Add(1)
			for _, event := range publicResponsesTestEvents {
				if err = conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
					return
				}
			}
		}
	}))
	defer server.Close()
	token := "first-key"
	network := config.NetworkConfig{}
	model := publicResponsesTestModel(t, server, func(context.Context) (publicResponsesPolicy, error) {
		if token == "" {
			return publicResponsesPolicy{}, errors.New("logged out")
		}
		return publicResponsesPolicy{apiKey: token, network: network, client: publicResponsesLocalClient(server)}, nil
	})
	for step := 0; step < 3; step++ {
		if step == 1 {
			token = "second-key"
		}
		if step == 2 {
			network.Proxy = config.ProxyNone
		}
		_, err := collectPublicResponsesStream(publicResponsesTestContext(t), model, fantasy.Call{})
		require.NoError(t, err)
		require.Equal(t, "Bearer "+token, (<-headers).Get("Authorization"))
	}
	token = ""
	_, err := collectPublicResponsesStream(publicResponsesTestContext(t), model, fantasy.Call{})
	require.Error(t, err)
	for range 3 {
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			t.Fatal("revoked connection remained open")
		}
	}
	require.EqualValues(t, 3, requests.Load())
	require.Zero(t, httpRequests.Load())
}

func TestPublicResponsesWebSocketConsumerStopClosesConnection(t *testing.T) {
	entered, closed := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if _, _, err = conn.ReadMessage(); err != nil {
			return
		}
		close(entered)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(publicResponsesTestEvents[1]))
		_, _, _ = conn.ReadMessage()
		close(closed)
	}))
	defer server.Close()
	model := publicResponsesTestModel(t, server, func(context.Context) (publicResponsesPolicy, error) {
		return publicResponsesPolicy{apiKey: "public-test-key", client: publicResponsesLocalClient(server)}, nil
	})
	stream, err := model.Stream(publicResponsesTestContext(t), fantasy.Call{})
	require.NoError(t, err)
	for range stream {
		break
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach peer")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("consumer stop did not close peer")
	}
}
