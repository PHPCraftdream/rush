package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestPublicResponsesWebSocketDeclinedFinishForcesReconnect(t *testing.T) {
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
			for _, event := range publicResponsesTestEvents {
				if err = conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
					return
				}
			}
		}
	}))
	defer server.Close()
	model := publicResponsesTestModel(t, server, func(context.Context) (publicResponsesPolicy, error) {
		return publicResponsesPolicy{apiKey: "public-test-key", client: publicResponsesLocalClient(server)}, nil
	})
	ctx, cancel := context.WithTimeout(publicResponsesTestContext(t), 4*time.Second)
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
	require.True(t, sawFinish, "first stream must reach Finish before its consumer declines it")
	parts, streamErr := collectPublicResponsesStream(ctx, model, fantasy.Call{})
	require.NoError(t, streamErr)
	require.NotEmpty(t, parts)
	require.Equal(t, fantasy.StreamPartTypeFinish, parts[len(parts)-1].Type)
	require.EqualValues(t, 2, requests.Load())
	require.EqualValues(t, 2, upgrades.Load(), "abandoning Finish must retire the socket")
}

func TestPublicResponsesWebSocketIncompleteCannotSeedNextExchange(t *testing.T) {
	var requests, upgrades atomic.Int32
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
			if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.incomplete","response":{"id":"resp-incomplete","status":"incomplete","output":[]}}`)); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	model := publicResponsesTestModel(t, server, func(context.Context) (publicResponsesPolicy, error) {
		return publicResponsesPolicy{apiKey: "public-test-key", client: publicResponsesLocalClient(server)}, nil
	})
	ctx, cancel := context.WithTimeout(publicResponsesTestContext(t), 4*time.Second)
	defer cancel()
	for range 2 {
		parts, err := collectPublicResponsesStream(ctx, model, fantasy.Call{})
		require.NoError(t, err)
		require.NotEmpty(t, parts)
		require.Equal(t, fantasy.StreamPartTypeFinish, parts[len(parts)-1].Type)
	}
	require.EqualValues(t, 2, requests.Load())
	require.EqualValues(t, 2, upgrades.Load(), "incomplete response cannot establish a parent")
}

func TestPublicResponsesObjectFinishAcceptanceOwnsReuse(t *testing.T) {
	for _, acceptFinish := range []bool{false, true} {
		name := "declined"
		if acceptFinish {
			name = "accepted"
		}
		t.Run(name, func(t *testing.T) {
			var requests, upgrades atomic.Int32
			objectEvents := []string{
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"obj-msg","role":"assistant","status":"in_progress","content":[]}}`,
				`{"type":"response.output_text.delta","item_id":"obj-msg","output_index":0,"content_index":0,"delta":"{\"x\":1}"}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"obj-msg","role":"assistant","status":"completed","content":[{"type":"output_text","text":"{\"x\":1}"}]}}`,
				`{"type":"response.completed","response":{"id":"obj-response","status":"completed","output":[{"type":"message","id":"obj-msg","role":"assistant","status":"completed","content":[{"type":"output_text","text":"{\"x\":1}"}]}]}}`,
			}
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
					for _, event := range objectEvents {
						if err = conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
							return
						}
					}
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(publicResponsesTestContext(t), 4*time.Second)
			defer cancel()
			model := publicResponsesTestModel(t, server, func(context.Context) (publicResponsesPolicy, error) {
				return publicResponsesPolicy{apiKey: "public-test-key", client: publicResponsesLocalClient(server)}, nil
			})
			objectStream, err := model.StreamObject(ctx, fantasy.ObjectCall{
				Schema: fantasy.Schema{Type: "object", Properties: map[string]*fantasy.Schema{"x": {Type: "integer"}}},
			})
			require.NoError(t, err)
			sawFinish := false
			objectStream(func(part fantasy.ObjectStreamPart) bool {
				require.NotEqual(t, fantasy.ObjectStreamPartTypeError, part.Type, "object stream must not yield an error")
				if part.Type == fantasy.ObjectStreamPartTypeFinish {
					sawFinish = true
					return acceptFinish
				}
				return true
			})
			require.True(t, sawFinish, "object stream must reach ObjectFinish")
			parts, streamErr := collectPublicResponsesStream(ctx, model, fantasy.Call{})
			require.NoError(t, streamErr)
			require.NotEmpty(t, parts)
			require.Equal(t, fantasy.StreamPartTypeFinish, parts[len(parts)-1].Type)
			require.EqualValues(t, 2, requests.Load())
			wantUpgrades := int32(1)
			if !acceptFinish {
				wantUpgrades = 2
			}
			require.EqualValues(t, wantUpgrades, upgrades.Load(), "ObjectFinish acceptance controls socket reuse")
		})
	}
}
