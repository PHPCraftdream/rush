package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/provider/nettransport"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/responsesws"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// publicResponsesPolicy aliases the responsesws boundary type so retained
// coordinator-facing tests keep their original literals.
type publicResponsesPolicy = responsesws.PublicResponsesPolicy

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

func publicResponsesCapture[T any](t *testing.T, ctx context.Context, capture <-chan T, label string) T {
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

func publicResponsesTestModel(t *testing.T, server *httptest.Server, current func(context.Context) (publicResponsesPolicy, error), owners ...*coordinator) fantasy.LanguageModel {
	t.Helper()
	manager := nettransport.NewResponsesWSManager(t.Context(), 4)
	if len(owners) > 0 {
		owners[0].responsesWS = manager
	}
	t.Cleanup(func() { _ = manager.Close() })
	p, err := responsesws.NewPublicResponsesProvider("https://api.openai.com/v1", "public-test-key", nil, manager, strings.Replace(server.URL, "http://", "ws://", 1), current, nil)
	require.NoError(t, err)
	model, err := p.LanguageModel(t.Context(), "gpt-5.4")
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

func publicResponsesHistoryDecode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value map[string]any
	require.NoError(t, decoder.Decode(&value))
	return value
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
		return publicResponsesPolicy{APIKey: "public-test-key", Client: publicResponsesLocalClient(server)}, nil
	})
	retry := fantasy.RetryWithExponentialBackoffRespectingRetryHeaders[[]fantasy.StreamPart](fantasy.RetryOptions{MaxRetries: 3, InitialDelayIn: time.Nanosecond})
	_, err := retry(publicResponsesTestContext(t), func() ([]fantasy.StreamPart, error) {
		return collectPublicResponsesStream(publicResponsesTestContext(t), model, fantasy.Call{})
	})
	require.Error(t, err)
	require.True(t, modelkit.ResponsesWSReplayForbidden(err))
	require.Equal(t, classTerminal, classifyProviderError(err))
	joined := errors.Join(&fantasy.ProviderError{StatusCode: http.StatusUnauthorized}, err)
	require.False(t, modelkit.IsUnauthorized(joined))
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
	coord := &coordinator{currentAgent: &responsesWSCancelAgent{mockSessionAgent: &mockSessionAgent{}, cancel: cancel}, subAgentDrivers: toolwrap.NewRegistry()}
	BindCoordinatorBatchCutoverFacts(coord, responsesWSCancelFacts{})
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(coord) })
	model := publicResponsesTestModel(t, server, func(context.Context) (publicResponsesPolicy, error) {
		return publicResponsesPolicy{APIKey: "public-test-key", Client: publicResponsesLocalClient(server)}, nil
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
		require.True(t, modelkit.ResponsesWSReplayForbidden(err))
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not unwind exchange")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not close peer")
	}
	// The peer sees the socket close before the cancelled attempt releases its lease (the
	// AfterFunc closes the connection, then resolves the body). In that instant a new request
	// finds the conversation connection busy and takes the HTTP fallback by design; retry until
	// the lease is released, which is the property under test (a later request works over WS).
	var err error
	for deadline := time.Now().Add(2 * time.Second); ; {
		_, err = collectPublicResponsesStream(publicResponsesTestContext(t), model, fantasy.Call{})
		if err == nil || time.Now().After(deadline) {
			break
		}
		httpRequests.Store(0)
		time.Sleep(5 * time.Millisecond)
	}
	require.NoError(t, err)
	require.EqualValues(t, 2, requests.Load())
	coord.CancelAll()
	require.True(t, coord.responsesWS.Closed())
	_, err = collectPublicResponsesStream(publicResponsesTestContext(t), model, fantasy.Call{})
	require.Error(t, err)
	require.EqualValues(t, 2, requests.Load())
	require.Zero(t, httpRequests.Load())
}
