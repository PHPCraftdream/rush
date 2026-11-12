package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
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

const (
	publicChainTools  = `"tools":[{"type":"function","name":"lookup","description":"long schema description to make the continuation strictly shorter than the full request"}]`
	publicChainFirst  = `{"model":"gpt-5.4","store":false,"stream":true,` + publicChainTools + `,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}]}`
	publicChainSecond = `{"model":"gpt-5.4","store":false,"stream":true,` + publicChainTools + `,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"follow-up"}]}]}`
	publicChainDone   = `{"type":"response.completed","response":{"id":"resp-chain-1","status":"completed","output":[]}}`
	publicChainReject = `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"previous_response_not_found","param":"previous_response_id","message":"missing parent"}}`
)

// publicChainBridge serves script(n) events for the n-th request over
// WebSocket and returns the bridge, a bounded context, the captured frames
// and the number of HTTP fallbacks.
func publicChainBridge(t *testing.T, script func(n int) []string) (*publicResponsesClient, context.Context, chan []byte, *atomic.Int32) {
	t.Helper()
	frames := make(chan []byte, 8)
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, raw, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			frames <- raw
			for _, event := range script(int(count.Add(1))) {
				if conn.WriteMessage(websocket.TextMessage, []byte(event)) != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), cliprovider.SessionIDContextKey, "chain-session"), 5*time.Second)
	t.Cleanup(cancel)
	manager := nettransport.NewResponsesWSManager(ctx, 2)
	t.Cleanup(func() { _ = manager.Close() })
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
	return client, ctx, frames, &httpFallbacks
}

// publicChainExchange runs one request and returns the body read error.
func publicChainExchange(t *testing.T, ctx context.Context, client *publicResponsesClient, payload string) error {
	t.Helper()
	attempt := newPublicResponsesAttempt(nil, "", true)
	req, err := http.NewRequestWithContext(context.WithValue(ctx, publicResponsesAttemptKey{}, attempt), http.MethodPost, "https://api.openai.com/v1/responses", bytes.NewReader([]byte(payload)))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer public-test-key")
	response, err := client.Do(req)
	if err != nil {
		attempt.resolve(false)
		return err
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	attempt.resolve(readErr == nil)
	_ = response.Body.Close()
	return readErr
}

func publicChainFrame(t *testing.T, ctx context.Context, frames <-chan []byte) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(responsesDeltaCapture(t, ctx, frames), &fields))
	return fields
}

func TestPublicResponsesBridgeRejectedChainIsRecoveredOnceAndOnlyBeforeOutput(t *testing.T) {
	t.Run("recovered before output", func(t *testing.T) {
		client, ctx, frames, httpFallbacks := publicChainBridge(t, func(n int) []string {
			if n == 2 {
				return []string{publicChainReject}
			}
			return []string{publicChainDone}
		})
		require.NoError(t, publicChainExchange(t, ctx, client, publicChainFirst))
		require.NoError(t, publicChainExchange(t, ctx, client, publicChainSecond), "the rejected parent must not reach the caller")
		require.NotContains(t, publicChainFrame(t, ctx, frames), "previous_response_id")
		require.JSONEq(t, `"resp-chain-1"`, string(publicChainFrame(t, ctx, frames)["previous_response_id"]))
		recovered := publicChainFrame(t, ctx, frames)
		require.NotContains(t, recovered, "previous_response_id")
		var input []json.RawMessage
		require.NoError(t, json.Unmarshal(recovered["input"], &input))
		require.Len(t, input, 2, "the recovery carries the complete history")
		require.Zero(t, httpFallbacks.Load())
	})
	t.Run("not recovered twice", func(t *testing.T) {
		client, ctx, frames, httpFallbacks := publicChainBridge(t, func(n int) []string {
			if n == 1 {
				return []string{publicChainDone}
			}
			return []string{publicChainReject}
		})
		require.NoError(t, publicChainExchange(t, ctx, client, publicChainFirst))
		err := publicChainExchange(t, ctx, client, publicChainSecond)
		var exchangeErr *nettransport.ResponsesWSError
		require.ErrorAs(t, err, &exchangeErr)
		require.True(t, exchangeErr.NoReplay())
		require.Len(t, frames, 3, "first, chained, and exactly one full recovery")
		require.Zero(t, httpFallbacks.Load())
	})
	t.Run("a full frame is never recovered", func(t *testing.T) {
		client, ctx, frames, _ := publicChainBridge(t, func(int) []string { return []string{publicChainReject} })
		err := publicChainExchange(t, ctx, client, publicChainFirst)
		var exchangeErr *nettransport.ResponsesWSError
		require.ErrorAs(t, err, &exchangeErr)
		require.Len(t, frames, 1)
	})
	t.Run("output before the rejection is never replayed", func(t *testing.T) {
		client, ctx, frames, _ := publicChainBridge(t, func(n int) []string {
			if n == 2 {
				return []string{`{"type":"response.output_text.delta","delta":"partial"}`, publicChainReject}
			}
			return []string{publicChainDone}
		})
		require.NoError(t, publicChainExchange(t, ctx, client, publicChainFirst))
		err := publicChainExchange(t, ctx, client, publicChainSecond)
		var exchangeErr *nettransport.ResponsesWSError
		require.ErrorAs(t, err, &exchangeErr)
		require.Len(t, frames, 2, "a chained frame that produced output is not resent")
	})
}

func TestPublicResponsesBridgeTransportRecordsAreComparable(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	client, ctx, _, _ := publicChainBridge(t, func(n int) []string {
		if n == 2 {
			return []string{publicChainReject}
		}
		return []string{publicChainDone}
	})
	require.NoError(t, publicChainExchange(t, ctx, client, publicChainFirst))
	require.NoError(t, publicChainExchange(t, ctx, client, publicChainSecond))

	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		if record["msg"] == "responses transport exchange" {
			records = append(records, record)
		}
	}
	require.Len(t, records, 2)
	for _, record := range records {
		require.Equal(t, "websocket", record["transport"])
		require.Equal(t, "openai", record["provider"])
		require.NotEmpty(t, record["session_hash"])
		require.NotContains(t, logs.String(), "chain-session", "only the session hash may be logged")
		require.NotContains(t, logs.String(), "public-test-key")
	}
	require.EqualValues(t, 0, records[0]["chain_recoveries"])
	require.EqualValues(t, 1, records[1]["chain_recoveries"])
	require.Equal(t, "full", records[1]["frame_mode"])
	require.EqualValues(t, 1, records[1]["exchange_seq"], "the recovery ran on a fresh connection")
}
