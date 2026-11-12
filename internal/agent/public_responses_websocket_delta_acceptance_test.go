package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/nettransport"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type responsesDeltaNoHTTP struct{ calls *atomic.Int32 }

func (r responsesDeltaNoHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Host, "api.openai.com") {
		r.calls.Add(1)
	}
	return nil, errors.New("unexpected public Responses HTTP fallback")
}

func responsesDeltaCapture[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case item := <-ch:
		return item
	case <-ctx.Done():
		t.Fatalf("bounded WebSocket capture timed out: %v", ctx.Err())
		var zero T
		return zero
	}
}

func responsesDeltaDecode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	require.NoError(t, decoder.Decode(&value))
	return value
}

func TestPublicResponsesBridgeExactOutputDeltaAndMissingParentRecoversWithFullFrame(t *testing.T) {
	const first = `{"model":"gpt-5.4","store":false,"stream":true,"tools":[{"type":"function","name":"lookup","description":"long schema description to make the exact continuation strictly shorter than the full request"}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}]}`
	const functionOutput = `{"type":"function_call","id":"fc-exact-1","call_id":"call-exact-1","name":"lookup","arguments":"{ \"n\" : 9007199254740993 }"}`
	const reasoningOutput = `{"type":"reasoning","id":"rs-exact-1","summary":[{"type":"summary_text","text":"opaque"}],"encrypted_content":"opaque-encrypted-payload"}`
	const second = `{"model":"gpt-5.4","store":false,"stream":true,"tools":[{"type":"function","name":"lookup","description":"long schema description to make the exact continuation strictly shorter than the full request"}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},{"type":"function_call","id":"fc-exact-1","call_id":"call-exact-1","name":"lookup","arguments":"{ \"n\" : 9007199254740993 }"},{"type":"reasoning","id":"rs-exact-1","summary":[{"type":"summary_text","text":"opaque"}],"encrypted_content":"opaque-encrypted-payload"},{"type":"function_call_output","call_id":"call-exact-1","output":"tool result"},{"type":"message","role":"user","content":[{"type":"input_text","text":"follow-up"}]}]}`
	requests := make(chan []byte, 3)
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			_, raw, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			requests <- raw
			switch count.Add(1) {
			case 3:
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-exact-2","status":"completed","output":[]}}`))
				continue
			case 1:
				for index, item := range []string{functionOutput, reasoningOutput} {
					event := `{"type":"response.output_item.done","output_index":` + string(rune('0'+index)) + `,"item":` + item + `}`
					if err = conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
						return
					}
				}
				event := `{"type":"response.completed","response":{"id":"resp-exact-1","status":"completed","output":[` + functionOutput + `,` + reasoningOutput + `]}}`
				if err = conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
					return
				}
				continue
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"previous_response_not_found","param":"previous_response_id","message":"missing parent"}}`))
			return
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), cliprovider.SessionIDContextKey, "delta-session"), 4*time.Second)
	defer cancel()
	manager := nettransport.NewResponsesWSManager(ctx, 2)
	defer func() { _ = manager.Close() }()
	var httpFallbacks atomic.Int32
	client := &publicResponsesClient{
		manager:  manager,
		endpoint: strings.Replace(server.URL, "http://", "ws://", 1),
		current: func(context.Context) (publicResponsesPolicy, error) {
			return publicResponsesPolicy{
				apiKey: "public-test-key", network: nettransport.ResolveNetworkConfig(nil, &config.NetworkConfig{Proxy: config.ProxyNone}),
				transport: config.ResponsesTransportAuto,
				client:    &http.Client{Transport: responsesDeltaNoHTTP{calls: &httpFallbacks}},
			}, nil
		},
	}
	for index, payload := range [][]byte{[]byte(first), []byte(second)} {
		attempt := newPublicResponsesAttempt(nil, "", true)
		req, err := http.NewRequestWithContext(context.WithValue(ctx, publicResponsesAttemptKey{}, attempt), http.MethodPost, "https://api.openai.com/v1/responses", bytes.NewReader(payload))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer public-test-key")
		response, err := client.Do(req)
		require.NoError(t, err)
		_, readErr := io.Copy(io.Discard, response.Body)
		require.NoError(t, readErr, "request %d: a rejected parent must be recovered, not surfaced", index)
		attempt.resolve(true)
		_ = response.Body.Close()
	}
	wireFirst := responsesDeltaCapture(t, ctx, requests)
	wireSecond := responsesDeltaCapture(t, ctx, requests)
	wireRecovered := responsesDeltaCapture(t, ctx, requests)
	fullFirst, err := nettransport.ResponsesWSFullFrame([]byte(first))
	require.NoError(t, err)
	fullSecond, err := nettransport.ResponsesWSFullFrame([]byte(second))
	require.NoError(t, err)
	require.Equal(t, fullFirst, wireFirst)
	require.Less(t, len(wireSecond), len(fullSecond))
	firstWire := responsesDeltaDecode(t, wireFirst)
	secondWire := responsesDeltaDecode(t, wireSecond)
	fullFirstFields := responsesDeltaDecode(t, fullFirst)
	fullFields := responsesDeltaDecode(t, fullSecond)
	require.Equal(t, fullFirstFields, firstWire)
	require.NotContains(t, firstWire, "stream")
	require.NotContains(t, firstWire, "background")
	require.NotContains(t, secondWire, "stream")
	require.NotContains(t, secondWire, "background")
	var deltaFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wireSecond, &deltaFields))
	require.Equal(t, `"resp-exact-1"`, string(deltaFields["previous_response_id"]))

	// Reconstruct the complete logical frame from the original prefix, actual
	// completed output items, and wire suffix; UseNumber preserves large integers.
	fullInputs, ok := fullFields["input"].([]any)
	require.True(t, ok)
	deltaInputs, ok := secondWire["input"].([]any)
	require.True(t, ok)
	var wireFirstFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wireFirst, &wireFirstFields))
	var firstInputs, suffix, prior []json.RawMessage
	require.NoError(t, json.Unmarshal(wireFirstFields["input"], &firstInputs))
	require.NoError(t, json.Unmarshal(deltaFields["input"], &suffix))
	require.NoError(t, json.Unmarshal(json.RawMessage("["+functionOutput+","+reasoningOutput+"]"), &prior))
	reconstructed := append(append(append([]json.RawMessage(nil), firstInputs...), prior...), suffix...)
	var reconstructedRaw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wireSecond, &reconstructedRaw))
	reconstructedRaw["input"], err = json.Marshal(reconstructed)
	require.NoError(t, err)
	delete(reconstructedRaw, "previous_response_id")
	encoded, err := json.Marshal(reconstructedRaw)
	require.NoError(t, err)
	require.Equal(t, fullFields, responsesDeltaDecode(t, encoded))
	require.Len(t, fullInputs, 5)
	require.Len(t, deltaInputs, 2)
	require.Zero(t, httpFallbacks.Load())
	require.Equal(t, fullSecond, wireRecovered, "the recovery is exactly the full frame, on a fresh connection")
	require.EqualValues(t, 3, count.Load())
}
