package codexprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/nettransport"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func codexAcceptanceFrame(t *testing.T, ctx context.Context, frames <-chan []byte) []byte {
	t.Helper()
	select {
	case frame := <-frames:
		return frame
	case <-ctx.Done():
		t.Fatalf("waiting for Codex WebSocket frame: %v", ctx.Err())
		return nil
	}
}

func TestCodexFantasySerializerMismatchUsesFullNetworkRequest(t *testing.T) {
	var upgrades, requests atomic.Int32
	frames := make(chan []byte, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrades.Add(1)
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
			frames <- frame
			if requests.Add(1) == 1 {
				event := `{"type":"response.completed","response":{"id":"resp-prefix","status":"completed","output":[{"type":"function_call","id":"fc-prefix","call_id":"call-prefix","name":"lookup","arguments":"{ \"n\" : 9007199254740993 }"}]}}`
				if err = conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
					return
				}
				continue
			}
			event := `{"type":"response.completed","response":{"id":"resp-followup","status":"completed","output":[{"type":"message","id":"msg-followup","content":[{"type":"output_text","text":"done"}]}]}}`
			if err = conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(websocketCodexContext(t), 4*time.Second)
	defer cancel()
	model := websocketCodexModel(t, server, newTestSource())
	first, err := model.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("first")}})
	require.NoError(t, err)
	require.Equal(t, "call-prefix", first.Content.ToolCalls()[0].ToolCallID)
	followup := fantasy.Call{Prompt: fantasy.Prompt{
		fantasy.NewUserMessage("first"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "call-prefix", ToolName: "lookup", Input: `{ "n" : 9007199254740993 }`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "call-prefix", Output: fantasy.ToolResultOutputContentText{Text: "tool result"}},
		}},
		fantasy.NewUserMessage("follow-up"),
	}}
	_, err = model.Generate(ctx, followup)
	require.NoError(t, err)
	firstFrame := codexAcceptanceFrame(t, ctx, frames)
	secondFrame := codexAcceptanceFrame(t, ctx, frames)
	body, err := makeRequestBody(ctx, "gpt-5-codex", followup)
	require.NoError(t, err)
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	expected, err := nettransport.ResponsesWSCodexFullFrame(payload)
	require.NoError(t, err)
	require.Equal(t, expected, secondFrame, "Codex must send the full serialized Fantasy history")
	var sent map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(secondFrame, &sent))
	require.NotContains(t, sent, "previous_response_id", "Fantasy history omits output item IDs and encrypted reasoning")
	require.Contains(t, sent, "stream")
	require.EqualValues(t, 2, requests.Load())
	require.EqualValues(t, 1, upgrades.Load(), "accepted full request may reuse its socket")
	require.NotEmpty(t, firstFrame)
}

func TestCodexStreamDeclinedFinishForcesReconnect(t *testing.T) {
	var upgrades, requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrades.Add(1)
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			if _, _, err = conn.ReadMessage(); err != nil {
				return
			}
			requests.Add(1)
			if err = conn.WriteMessage(websocket.TextMessage, []byte(websocketCodexCompletion)); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	model := websocketCodexModel(t, server, newTestSource())
	ctx, cancel := context.WithTimeout(websocketCodexContext(t), 4*time.Second)
	defer cancel()
	stream, err := model.Stream(ctx, fantasy.Call{})
	require.NoError(t, err)
	sawFinish := false
	stream(func(part fantasy.StreamPart) bool {
		require.NotEqual(t, fantasy.StreamPartTypeError, part.Type, "stream must not yield an error")
		if part.Type == fantasy.StreamPartTypeFinish {
			sawFinish = true
			return false
		}
		return true
	})
	require.True(t, sawFinish)
	_, err = model.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("follow-up")}})
	require.NoError(t, err)
	require.EqualValues(t, 2, requests.Load())
	require.EqualValues(t, 2, upgrades.Load(), "declining final Finish must force reconnect")
}

func TestCodexObjectFinishAcceptanceOwnsReuse(t *testing.T) {
	for _, acceptFinish := range []bool{false, true} {
		name := "declined"
		if acceptFinish {
			name = "accepted"
		}
		t.Run(name, func(t *testing.T) {
			var upgrades, requests atomic.Int32
			frames := make(chan []byte, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upgrades.Add(1)
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
					frames <- frame
					if requests.Add(1) == 1 {
						for _, event := range []string{
							`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"obj-fc","call_id":"obj-call","name":"generate_object","arguments":""}}`,
							`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"obj-fc","delta":"{\"n\":1}"}`,
							`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"obj-fc","call_id":"obj-call","name":"generate_object","arguments":"{\"n\":1}"}}`,
							`{"type":"response.completed","response":{"id":"resp-object","status":"completed","output":[{"type":"function_call","id":"obj-fc","call_id":"obj-call","name":"generate_object","arguments":"{\"n\":1}"}]}}`,
						} {
							if err = conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
								return
							}
						}
						continue
					}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-second","status":"completed","output":[{"type":"message","id":"msg-second","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}`))
				}
			}))
			defer server.Close()
			model := websocketCodexModel(t, server, newTestSource())
			ctx, cancel := context.WithTimeout(websocketCodexContext(t), 4*time.Second)
			defer cancel()
			call := fantasy.ObjectCall{Schema: fantasy.Schema{Type: "object", Properties: map[string]*fantasy.Schema{"n": {Type: "integer"}}}}
			stream, err := model.StreamObject(ctx, call)
			require.NoError(t, err)
			sawFinish := false
			stream(func(part fantasy.ObjectStreamPart) bool {
				require.NotEqual(t, fantasy.ObjectStreamPartTypeError, part.Type, "object stream must not yield an error")
				if part.Type == fantasy.ObjectStreamPartTypeFinish {
					sawFinish = true
					return acceptFinish
				}
				return true
			})
			require.True(t, sawFinish)
			nextCall := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("next")}}
			_, err = model.Generate(ctx, nextCall)
			require.NoError(t, err)
			firstFrame := codexAcceptanceFrame(t, ctx, frames)
			secondFrame := codexAcceptanceFrame(t, ctx, frames)
			body, err := makeRequestBody(ctx, "gpt-5-codex", nextCall)
			require.NoError(t, err)
			payload, err := json.Marshal(body)
			require.NoError(t, err)
			expected, err := nettransport.ResponsesWSCodexFullFrame(payload)
			require.NoError(t, err)
			require.Equal(t, expected, secondFrame)
			var wire map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(secondFrame, &wire))
			require.NotContains(t, wire, "previous_response_id")
			require.NotEmpty(t, firstFrame)
			require.EqualValues(t, 2, requests.Load())
			wantUpgrades := int32(1)
			if !acceptFinish {
				wantUpgrades = 2
			}
			require.EqualValues(t, wantUpgrades, upgrades.Load())
		})
	}
}
