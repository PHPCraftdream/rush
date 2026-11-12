package codexprovider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/nettransport"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

const (
	wsFailureCompleted = `{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[]}}`
	wsFailureLifecycle = `{"type":"response.in_progress","response":{"id":"resp-r","status":"in_progress","output":[],"usage":null}}`
	wsFailureLongText  = "retained prefix for delta framing "
)

type wsFailurePeer struct {
	frames                           chan []byte
	upgrades, requests, httpRequests atomic.Int32
}

// newWSFailurePeer answers the n-th request (1-based) with script(n) over one
// WebSocket per upgrade; the isolated env keeps real global config untouched.
func newWSFailurePeer(t *testing.T, script func(n int) []string) (*httptest.Server, *wsFailurePeer) {
	t.Helper()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(key, "")
	}
	peer := &wsFailurePeer{frames: make(chan []byte, 16)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			peer.httpRequests.Add(1)
			http.Error(w, "unexpected HTTP fallback", http.StatusInternalServerError)
			return
		}
		peer.upgrades.Add(1)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, frame, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			n := int(peer.requests.Add(1))
			select {
			case peer.frames <- append([]byte(nil), frame...):
			default:
			}
			for _, event := range script(n) {
				if conn.WriteMessage(websocket.TextMessage, []byte(event)) != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("ws peer: upgrades=%d requests=%d http=%d", peer.upgrades.Load(), peer.requests.Load(), peer.httpRequests.Load())
		}
	})
	return server, peer
}

func (p *wsFailurePeer) frame(t *testing.T) map[string]any {
	t.Helper()
	select {
	case frame := <-p.frames:
		return codexDeltaFrameJSONValue(t, frame)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a captured WebSocket frame")
		return nil
	}
}

func wsFailureCall(extra ...string) fantasy.Call {
	prompt := fantasy.Prompt{fantasy.NewUserMessage(strings.Repeat(wsFailureLongText, 12))}
	for _, text := range extra {
		prompt = append(prompt, fantasy.NewUserMessage(text))
	}
	return fantasy.Call{Prompt: prompt}
}

func wsFailureContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(websocketCodexContext(t), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

type contextRejectionMarker interface{ CodexContextRejectedBeforeOutput() bool }

func requireCertifiedRejection(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var marker contextRejectionMarker
	require.ErrorAs(t, err, &marker)
	require.True(t, marker.CodexContextRejectedBeforeOutput())
	var guard interface{ NoReplay() bool }
	require.False(t, errors.As(err, &guard), "a certified pre-output rejection must not be marked no-replay: %v", err)
	var terminal *nettransport.ResponsesWSError
	require.False(t, errors.As(err, &terminal))
	var providerErr *fantasy.ProviderError
	require.ErrorAs(t, err, &providerErr)
	require.True(t, providerErr.IsContextTooLarge())
}

func requireNoReplayWithoutMarker(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var guard interface{ NoReplay() bool }
	require.ErrorAs(t, err, &guard)
	require.True(t, guard.NoReplay())
	var marker contextRejectionMarker
	if errors.As(err, &marker) {
		require.False(t, marker.CodexContextRejectedBeforeOutput())
	}
}

func wsFailureRetry(ctx context.Context, model fantasy.LanguageModel, call fantasy.Call) error {
	retry := fantasy.RetryWithExponentialBackoffRespectingRetryHeaders[*fantasy.Response](fantasy.RetryOptions{MaxRetries: 3, InitialDelayIn: time.Nanosecond})
	_, err := retry(ctx, func() (*fantasy.Response, error) { return model.Generate(ctx, call) })
	return err
}

var wsContextRejections = []struct{ name, event string }{
	{"response.failed nested response error", `{"type":"response.failed","response":{"error":{"code":"context_length_exceeded","message":"request exceeds context"}}}`},
	{"error nested error", `{"type":"error","error":{"code":"context_length_exceeded","message":"request exceeds context"}}`},
	{"error flat fields", `{"type":"error","code":"context_length_exceeded","message":"request exceeds context"}`},
}

func TestCodexWebSocketCertifiedContextRejectionIsReplayableAndReconnects(t *testing.T) {
	for _, rejection := range wsContextRejections {
		t.Run(rejection.name+"/first frame", func(t *testing.T) {
			server, peer := newWSFailurePeer(t, func(n int) []string {
				if n == 1 {
					return []string{wsFailureLifecycle, rejection.event}
				}
				return []string{wsFailureCompleted}
			})
			model := websocketCodexModel(t, server, newTestSource())
			ctx := wsFailureContext(t)
			_, err := model.Generate(ctx, wsFailureCall())
			requireCertifiedRejection(t, err)
			_, err = model.Generate(ctx, wsFailureCall())
			require.NoError(t, err)
			require.NotContains(t, peer.frame(t), "previous_response_id")
			require.NotContains(t, peer.frame(t), "previous_response_id")
			require.EqualValues(t, 2, peer.requests.Load())
			require.EqualValues(t, 2, peer.upgrades.Load(), "the rejected connection must be retired")
			require.Zero(t, peer.httpRequests.Load())
		})
		t.Run(rejection.name+"/rejected delta frame", func(t *testing.T) {
			server, peer := newWSFailurePeer(t, func(n int) []string {
				if n == 2 {
					return []string{wsFailureLifecycle, rejection.event}
				}
				return []string{wsFailureCompleted}
			})
			model := websocketCodexModel(t, server, newTestSource())
			ctx := wsFailureContext(t)
			_, err := model.Generate(ctx, wsFailureCall())
			require.NoError(t, err)
			_, err = model.Generate(ctx, wsFailureCall("next"))
			requireCertifiedRejection(t, err)
			_, err = model.Generate(ctx, wsFailureCall("next"))
			require.NoError(t, err)
			require.NotContains(t, peer.frame(t), "previous_response_id")
			require.Equal(t, "resp-1", peer.frame(t)["previous_response_id"], "the second request must be a parent delta")
			third := peer.frame(t)
			require.NotContains(t, third, "previous_response_id", "stale parent state must not survive the rejection")
			require.Len(t, third["input"], 2, "the retry after rejection must be a full frame")
			require.EqualValues(t, 3, peer.requests.Load())
			require.EqualValues(t, 2, peer.upgrades.Load(), "the rejected connection must be retired")
			require.Zero(t, peer.httpRequests.Load())
		})
	}
}

func TestCodexWebSocketContextRejectionAfterOutputStaysNoReplay(t *testing.T) {
	const rejection = `{"type":"error","error":{"code":"context_length_exceeded","message":"request exceeds context"}}`
	for _, tc := range []struct {
		name   string
		events []string
	}{
		{"after text delta", []string{`{"type":"response.output_text.delta","delta":"partial"}`, rejection}},
		{"after output item", []string{`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg-1","role":"assistant","content":[]}}`, rejection}},
		{"after unknown event", []string{`{"type":"response.mystery.event"}`, rejection}},
		{"usage bearing failure", []string{`{"type":"response.failed","response":{"usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1},"error":{"code":"context_length_exceeded","message":"request exceeds context"}}}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, peer := newWSFailurePeer(t, func(int) []string { return tc.events })
			model := websocketCodexModel(t, server, newTestSource())
			err := wsFailureRetry(wsFailureContext(t), model, wsFailureCall())
			requireNoReplayWithoutMarker(t, err)
			require.EqualValues(t, 1, peer.requests.Load(), "a post-output failure must never be replayed")
			require.Zero(t, peer.httpRequests.Load())
		})
	}
}

func TestCodexWebSocketPreOutputProviderFailureIsVisibleButNotReplayable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		event   string
		message string
		context bool
	}{
		{"usage limit", `{"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`, "usage_limit_reached", false},
		{"rate limit", `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"Rate limit reached"}}}`, "rate_limit_exceeded", false},
		{"overloaded", `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"busy"}}}`, "server_is_overloaded", false},
		{"uncertified context rejection", `{"type":"error","error":{"code":"context_length_exceeded","message":"too long"},"extra":1}`, "context_length_exceeded", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, peer := newWSFailurePeer(t, func(int) []string { return []string{tc.event} })
			model := websocketCodexModel(t, server, newTestSource())
			err := wsFailureRetry(wsFailureContext(t), model, wsFailureCall())
			requireNoReplayWithoutMarker(t, err)
			var providerErr *fantasy.ProviderError
			require.ErrorAs(t, err, &providerErr)
			require.Contains(t, providerErr.Message, tc.message)
			require.Equal(t, tc.context, providerErr.IsContextTooLarge())
			require.False(t, providerErr.IsRetryable(), "the visible provider error must not re-enter Fantasy's retry loop")
			var terminal *nettransport.ResponsesWSError
			require.ErrorAs(t, err, &terminal)
			require.True(t, terminal.NoReplay())
			require.EqualValues(t, 1, peer.requests.Load())
			require.Zero(t, peer.httpRequests.Load())
		})
	}
}
