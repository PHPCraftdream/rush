package codexprovider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/nettransport"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type websocketCodexTestManager struct {
	*nettransport.ResponsesWSManager
}
type websocketCodexTestLease struct{ *nettransport.ResponsesWSLease }

func (l websocketCodexTestLease) Conn() WebSocketConnection { return l.ResponsesWSLease.Conn() }
func (m websocketCodexTestManager) Acquire(ctx context.Context, request WebSocketRequest) (WebSocketLease, error) {
	lease, err := m.ResponsesWSManager.Acquire(ctx, nettransport.ResponsesWSOptions{
		Protocol: "codex-responses", Endpoint: request.Endpoint, ReadPolicy: nettransport.ResponsesWSReadRequestContext,
		AuthToken: request.AuthToken, Headers: request.Headers, StillCurrent: request.StillCurrent,
		Owner: nettransport.ResponsesWSOwner{AccountID: request.AccountID, AuthRevision: request.AuthRevision, ConversationID: request.ConversationID},
	}, nettransport.ResponsesWSPlannedMaxAge)
	if err != nil {
		return nil, err
	}
	return websocketCodexTestLease{lease}, nil
}
func (websocketCodexTestManager) UnauthorizedHandshake(err error) bool {
	var handshake *nettransport.ResponsesWSHandshakeError
	return errors.As(err, &handshake) && handshake.Status == http.StatusUnauthorized
}

func websocketCodexModel(t *testing.T, server *httptest.Server, source TokenSource) fantasy.LanguageModel {
	t.Helper()
	manager := nettransport.NewResponsesWSManager(t.Context(), 2)
	t.Cleanup(func() { _ = manager.Close() })
	p, err := NewWithSource(newCodexTestClient(server), source, WebSocketOptions{Manager: websocketCodexTestManager{manager}, EncodeFrame: nettransport.ResponsesWSCodexFullFrame,
		endpoint: strings.Replace(server.URL, "http://", "ws://", 1), Current: func() (WebSocketPolicy, error) { return WebSocketPolicy{Transport: WebSocketTransportAuto}, nil }})
	require.NoError(t, err)
	model, err := p.LanguageModel(t.Context(), "gpt-5-codex")
	require.NoError(t, err)
	return model
}

func websocketCodexContext(t *testing.T) context.Context {
	return context.WithValue(t.Context(), cliprovider.SessionIDContextKey, "codex-ws-session")
}

const websocketCodexCompletion = `{"type":"response.completed","response":{"status":"completed","output":[{"id":"msg-1","type":"message","content":[{"type":"output_text","text":"answer"}]},{"id":"rs-1","type":"reasoning","summary":[{"type":"summary_text","text":"reason"}],"encrypted_content":"opaque"},{"id":"fc-1","type":"function_call","call_id":"call-1","name":"lookup","arguments":"{ \"n\": 9007199254740993 }"}],"usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}}`

func TestCodexWebSocketFullFrameParityAndSequentialReuse(t *testing.T) {
	var upgrades, wsRequests, httpRequests atomic.Int32
	frames := make(chan []byte, 8)
	headers := make(chan http.Header, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			httpRequests.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			writeCodexEvent(w, websocketCodexCompletion)
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
			_, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}
			wsRequests.Add(1)
			frames <- frame
			if err = conn.WriteMessage(websocket.TextMessage, []byte(websocketCodexCompletion)); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	source := newTestSource()
	model := websocketCodexModel(t, server, source)
	call := fantasy.Call{Prompt: fantasy.Prompt{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "question"}}}}}
	ctx := websocketCodexContext(t)
	httpModel := codexProviderForTest(t, server)
	reference, err := httpModel.Generate(ctx, call)
	require.NoError(t, err)
	for range 2 {
		result, err := model.Generate(ctx, call)
		require.NoError(t, err)
		require.Equal(t, reference, result)
		require.Equal(t, "reason", result.Content.ReasoningText())
		require.Equal(t, `{ "n": 9007199254740993 }`, result.Content.ToolCalls()[0].Input)
		var body map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(<-frames, &body))
		require.JSONEq(t, `"response.create"`, string(body["type"]))
		require.JSONEq(t, `true`, string(body["stream"]))
		require.JSONEq(t, `false`, string(body["store"]))
		require.NotContains(t, body, "background")
		require.NotContains(t, body, "previous_response_id")
	}
	h := <-headers
	// Verified in openai/codex 37103d7f01e82162472c6fba7b4088d5a1303321,
	// codex-rs/core/src/client.rs; this is NOT the public OpenAI contract.
	require.Equal(t, "responses_websockets=2026-02-06", h.Get("OpenAI-Beta"))
	require.Equal(t, "Bearer old-access", h.Get("Authorization"))
	require.Equal(t, "old-account", h.Get("chatgpt-account-id"))
	require.Equal(t, "codex-ws-session", h.Get("conversation_id"))
	require.EqualValues(t, 1, upgrades.Load())
	require.EqualValues(t, 2, wsRequests.Load())
	require.EqualValues(t, 1, httpRequests.Load())
}

func TestCodexWebSocketHandshakeRefreshAndLogout(t *testing.T) {
	var upgrades, requests, httpRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			httpRequests.Add(1)
			w.WriteHeader(500)
			return
		}
		upgrades.Add(1)
		if r.Header.Get("Authorization") == "Bearer old-access" {
			w.WriteHeader(401)
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
		requests.Add(1)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(websocketCodexCompletion))
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	source := newTestSource()
	source.refreshHook = func(context.Context, oauth.Token) error { source.set(rotatedToken(), true); return nil }
	model := websocketCodexModel(t, server, source)
	_, err := model.Generate(websocketCodexContext(t), fantasy.Call{})
	require.NoError(t, err)
	source.set(oauth.Token{}, false)
	_, err = model.Generate(websocketCodexContext(t), fantasy.Call{})
	require.Error(t, err)
	require.EqualValues(t, 1, source.refreshes.Load())
	require.EqualValues(t, 2, upgrades.Load())
	require.EqualValues(t, 1, requests.Load())
	require.Zero(t, httpRequests.Load())
}

func TestCodexWebSocketAmbiguousDisconnectCannotEnterFantasyRetryOrHTTP(t *testing.T) {
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
	model := websocketCodexModel(t, server, newTestSource())
	retry := fantasy.RetryWithExponentialBackoffRespectingRetryHeaders[*fantasy.Response](fantasy.RetryOptions{MaxRetries: 3, InitialDelayIn: time.Nanosecond})
	_, err := retry(websocketCodexContext(t), func() (*fantasy.Response, error) { return model.Generate(websocketCodexContext(t), fantasy.Call{}) })
	var terminal *nettransport.ResponsesWSError
	require.ErrorAs(t, err, &terminal)
	require.True(t, terminal.NoReplay())
	require.EqualValues(t, 1, requests.Load())
	require.Zero(t, httpRequests.Load())
}

func TestCodexWebSocketCancellationClosesPeerWithoutReplay(t *testing.T) {
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
		_, _, _ = conn.ReadMessage()
		close(closed)
	}))
	defer server.Close()
	model := websocketCodexModel(t, server, newTestSource())
	ctx, cancel := context.WithCancel(websocketCodexContext(t))
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := model.Generate(ctx, fantasy.Call{}); done <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach peer")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
		var terminal *nettransport.ResponsesWSError
		require.ErrorAs(t, err, &terminal)
		require.True(t, terminal.NoReplay())
	case <-time.After(2 * time.Second):
		t.Fatal("canceled exchange did not return")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not close peer")
	}
}
