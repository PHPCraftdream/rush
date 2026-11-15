package codexprovider

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var wsChainRejections = []struct{ name, event string }{
	{"nested error with status", `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"previous_response_not_found","param":"previous_response_id","message":"The specified previous_response_id was not found."}}`},
	{"flat error", `{"type":"error","code":"previous_response_not_found","message":"missing parent"}`},
	{"response failed", `{"type":"response.failed","response":{"error":{"code":"previous_response_not_found","message":"missing parent"}}}`},
}

func TestCodexWebSocketRejectedChainRecoversWithFullFrameOnFreshConnection(t *testing.T) {
	for _, rejection := range wsChainRejections {
		for _, lifecycle := range []bool{false, true} {
			name := rejection.name
			if lifecycle {
				name += " after lifecycle event"
			}
			t.Run(name, func(t *testing.T) {
				server, peer := newWSFailurePeer(t, func(n int) []string {
					if n == 2 {
						if lifecycle {
							return []string{wsFailureLifecycle, rejection.event}
						}
						return []string{rejection.event}
					}
					return []string{wsFailureCompleted}
				})
				model := websocketCodexModel(t, server, newTestSource())
				ctx := wsFailureContext(t)
				_, err := model.Generate(ctx, wsFailureCall())
				require.NoError(t, err)
				_, err = model.Generate(ctx, wsFailureCall("next"))
				require.NoError(t, err, "a missing parent must be recovered, not surfaced")
				_, err = model.Generate(ctx, wsFailureCall("next", "again"))
				require.NoError(t, err)

				require.NotContains(t, peer.frame(t), "previous_response_id")
				require.Equal(t, "resp-1", peer.frame(t)["previous_response_id"], "the second request is the chained frame the server rejected")
				recovered := peer.frame(t)
				require.NotContains(t, recovered, "previous_response_id", "the recovery must be a full frame")
				require.Len(t, recovered["input"], 2, "the recovery carries the complete history")
				peer.frame(t) // the request after recovery
				require.EqualValues(t, 4, peer.requests.Load())
				require.EqualValues(t, 2, peer.upgrades.Load(), "the rejected connection must be retired and replaced once")
				require.Zero(t, peer.httpRequests.Load(), "recovery stays on WebSocket")
			})
		}
	}
}

func TestCodexWebSocketChainRejectionIsNotRecoveredAfterOutputOrOnFullFrames(t *testing.T) {
	const rejection = `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"previous_response_not_found","param":"previous_response_id"}}`
	t.Run("after output", func(t *testing.T) {
		server, peer := newWSFailurePeer(t, func(n int) []string {
			if n == 2 {
				return []string{`{"type":"response.output_text.delta","delta":"partial"}`, rejection}
			}
			return []string{wsFailureCompleted}
		})
		model := websocketCodexModel(t, server, newTestSource())
		ctx := wsFailureContext(t)
		_, err := model.Generate(ctx, wsFailureCall())
		require.NoError(t, err)
		err = wsFailureRetry(ctx, model, wsFailureCall("next"))
		requireNoReplayWithoutMarker(t, err)
		require.EqualValues(t, 2, peer.requests.Load(), "output was already delivered, so the request must not be replayed")
	})
	t.Run("a full frame is never recovered", func(t *testing.T) {
		server, peer := newWSFailurePeer(t, func(int) []string { return []string{rejection} })
		model := websocketCodexModel(t, server, newTestSource())
		err := wsFailureRetry(wsFailureContext(t), model, wsFailureCall())
		requireNoReplayWithoutMarker(t, err)
		require.EqualValues(t, 1, peer.requests.Load())
	})
	t.Run("recovery is attempted once", func(t *testing.T) {
		server, peer := newWSFailurePeer(t, func(n int) []string {
			if n == 1 {
				return []string{wsFailureCompleted}
			}
			return []string{rejection}
		})
		model := websocketCodexModel(t, server, newTestSource())
		ctx := wsFailureContext(t)
		_, err := model.Generate(ctx, wsFailureCall())
		require.NoError(t, err)
		err = wsFailureRetry(ctx, model, wsFailureCall("next"))
		requireNoReplayWithoutMarker(t, err)
		require.EqualValues(t, 3, peer.requests.Load(), "first, chained, one full recovery, and nothing more")
	})
}

func TestCodexTransportRecordMakesWebSocketAndHTTPTurnsComparable(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	server, _ := newWSFailurePeer(t, func(n int) []string {
		if n == 2 {
			return []string{wsChainRejections[0].event}
		}
		return []string{wsFailureCompleted}
	})
	model := websocketCodexModel(t, server, newTestSource())
	ctx := wsFailureContext(t)
	_, err := model.Generate(ctx, wsFailureCall())
	require.NoError(t, err)
	_, err = model.Generate(ctx, wsFailureCall("next"))
	require.NoError(t, err)

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
		require.Equal(t, "finished", record["outcome"])
		require.Equal(t, "openai-codex", record["provider"])
		require.NotEmpty(t, record["session_hash"])
		require.NotEqual(t, "codex-ws-session", record["session_hash"], "only the hash of the session is logged")
		require.Contains(t, record, "frame_mode")
		require.Contains(t, record, "frame_wire_bytes")
	}
	require.EqualValues(t, 0, records[0]["chain_recoveries"])
	require.EqualValues(t, 1, records[1]["chain_recoveries"])
	require.Equal(t, "full", records[1]["frame_mode"], "the recovered exchange is reported as the full frame that completed it")
	require.EqualValues(t, 1, records[1]["exchange_seq"], "it ran on the fresh connection")
}
