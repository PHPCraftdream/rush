package nettransport

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

const (
	diagLongTool = `"tools":[{"name":"lookup","description":"A deliberately long tool description keeps the continuation frame strictly shorter than the full request frame.","parameters":{"type":"object"}}]`
	diagFirst    = `{"type":"response.create","model":"m","store":false,` + diagLongTool + `,"input":[{"role":"user","content":"first"}]}`
	diagDone     = `{"type":"response.completed","response":{"id":"resp-diag","status":"completed","output":[{"type":"function_call","id":"fc-1","name":"lookup","arguments":"{}"}]}}`
)

func diagState(t *testing.T) responsesWSDeltaState {
	t.Helper()
	return responsesWSDeltaRegressionSeed(t, diagFirst, diagDone, false)
}

func TestResponsesWSFrameInfoExplainsEveryFullFrameReason(t *testing.T) {
	exact := `{"type":"response.create","model":"m","store":false,` + diagLongTool + `,"input":[{"role":"user","content":"first"},{"type":"function_call","id":"fc-1","name":"lookup","arguments":"{}"},{"role":"user","content":"next"}]}`
	for _, tc := range []struct {
		name       string
		frame      string
		enabled    bool
		seeded     bool
		mode       string
		reason     string
		divergedAt int
		wireItems  int
	}{
		{"exact extension is a delta", exact, true, true, ResponsesWSFrameDelta, "", -1, 1},
		{"disabled", exact, false, true, ResponsesWSFrameFull, ResponsesWSFullDisabled, -1, -1},
		{"no parent", exact, true, false, ResponsesWSFrameFull, ResponsesWSFullNoParent, -1, 3},
		{"options changed", strings.Replace(exact, `"model":"m"`, `"model":"other"`, 1), true, true, ResponsesWSFrameFull, ResponsesWSFullOptionsChanged, -1, 3},
		{"output not replayed verbatim", strings.Replace(exact, `"id":"fc-1",`, "", 1), true, true, ResponsesWSFrameFull, ResponsesWSFullHistoryDiverged, 1, 3},
		{"prefix edited", strings.Replace(exact, `"content":"first"`, `"content":"edited"`, 1), true, true, ResponsesWSFrameFull, ResponsesWSFullHistoryDiverged, 0, 3},
		{"history shrank", `{"type":"response.create","model":"m","store":false,` + diagLongTool + `,"input":[{"role":"user","content":"first"}]}`, true, true, ResponsesWSFrameFull, ResponsesWSFullHistoryDiverged, 1, 1},
		{"server stored mode", strings.Replace(exact, `"store":false`, `"store":true`, 1), true, true, ResponsesWSFrameFull, ResponsesWSFullIneligible, -1, -1},
		{"not json", `{`, true, true, ResponsesWSFrameFull, ResponsesWSFullUnparseable, -1, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := responsesWSDeltaState{}
			if tc.seeded {
				state = diagState(t)
			}
			wire, _ := state.prepare([]byte(tc.frame), tc.enabled)
			info := state.last
			require.Equal(t, tc.mode, info.Mode)
			require.Equal(t, tc.reason, info.Reason)
			require.Equal(t, tc.divergedAt, info.DivergedAt)
			require.Equal(t, tc.wireItems, info.WireItems)
			require.Equal(t, len(tc.frame), info.FullBytes)
			require.Equal(t, len(wire), info.WireBytes)
			if tc.mode == ResponsesWSFrameDelta {
				require.Less(t, info.WireBytes, info.FullBytes)
			}
		})
	}
}

func TestResponsesWSChainRejectedRecognizesOnlyMissingParentErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"nested error", `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"previous_response_not_found","param":"previous_response_id"}}`, true},
		{"flat error", `{"type":"error","code":"previous_response_not_found"}`, true},
		{"response failed", `{"type":"response.failed","response":{"error":{"code":"previous_response_not_found"}}}`, true},
		{"other error code", `{"type":"error","error":{"code":"context_length_exceeded"}}`, false},
		{"code on a non-error event", `{"type":"response.output_text.delta","code":"previous_response_not_found"}`, false},
		{"not json", `previous_response_not_found`, false},
		{"empty", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ResponsesWSChainRejected([]byte(tc.raw)))
		})
	}
}

func diagCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &out
}

func TestResponsesWSConnReportsFrameDecisionAndLogsParityFields(t *testing.T) {
	logs := diagCapture(t)
	server := responsesWSTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(diagDone))
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	options := responsesWSTestOptions(server.URL)
	options.Protocol = "public-openai-responses"
	conn, err := DialResponsesWebSocket(ctx, options)
	require.NoError(t, err)
	defer conn.Close()
	exchange := func(frame string) ResponsesWSFrameInfo {
		require.NoError(t, conn.BeginExchange())
		require.False(t, conn.Chained(), "a new exchange has no frame decision yet")
		require.NoError(t, conn.SendRaw(ctx, []byte(frame)))
		info := conn.FrameInfo()
		_, err := conn.ReceiveRaw(ctx)
		require.NoError(t, err)
		require.NoError(t, conn.MarkCompleted())
		return info
	}
	first := exchange(diagFirst)
	require.Equal(t, ResponsesWSFrameFull, first.Mode)
	require.Equal(t, ResponsesWSFullFirstExchange, first.Reason)
	second := exchange(`{"type":"response.create","model":"m","store":false,` + diagLongTool + `,"input":[{"role":"user","content":"first"},{"type":"function_call","id":"fc-1","name":"lookup","arguments":"{}"},{"role":"user","content":"next"}]}`)
	require.Equal(t, ResponsesWSFrameDelta, second.Mode)
	require.True(t, conn.Chained())
	require.Less(t, second.WireBytes, second.FullBytes)

	var sent []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		if record["msg"] == "responses websocket frame sent" {
			sent = append(sent, record)
		}
	}
	require.Len(t, sent, 2)
	require.EqualValues(t, 1, sent[0]["exchange_seq"])
	require.Equal(t, ResponsesWSFullFirstExchange, sent[0]["frame_reason"])
	require.EqualValues(t, 2, sent[1]["exchange_seq"])
	require.Equal(t, ResponsesWSFrameDelta, sent[1]["frame_mode"])
	require.EqualValues(t, 1, sent[1]["conn_delta_exchanges"])
	require.EqualValues(t, second.WireBytes, sent[1]["frame_wire_bytes"])
	require.NotContains(t, logs.String(), "responses-test-secret", "diagnostics must never carry credentials")
	require.NotContains(t, logs.String(), "conversation-test", "the session is reported only as a hash")
	require.Contains(t, logs.String(), "responses websocket exchange completed")
}
