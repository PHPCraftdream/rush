package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestAPIMatrixOversizeRESTRejectedBeforeHandlerAndListenerSurvives(t *testing.T) {
	for _, ch := range apiMatrixChannels {
		t.Run(ch.name, func(t *testing.T) {
			f := newAPIMatrixFixture(t, ch)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.base+"/api/v1/runs", bytes.NewReader(bytes.Repeat([]byte("x"), 65)))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer "+f.token)
			request.Header.Set("Content-Type", "application/json")
			response, err := f.client().Do(request)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode)
			var problem map[string]any
			if response.Header.Get("Content-Type") == "application/problem+json" {
				require.NoError(t, json.Unmarshal(body, &problem))
				require.EqualValues(t, http.StatusRequestEntityTooLarge, problem["status"])
			}
			request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, f.base+"/api/v1/sessions", nil)
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer "+f.token)
			response, err = f.client().Do(request)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode)
			require.NoError(t, response.Body.Close())
		})
	}
}

func TestAPIMatrixOversizeWSMessageClosesConnectionAndListenerSurvives(t *testing.T) {
	for _, ch := range apiMatrixChannels {
		if !ch.ws {
			continue
		}
		t.Run(ch.name, func(t *testing.T) {
			f := newAPIMatrixFixture(t, ch)
			dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second, TLSClientConfig: f.tlsConfig(), Subprotocols: []string{"rush.v1", "rush.bearer." + f.token}}
			conn, response, err := dialer.Dial(f.wsURL, nil)
			require.NoError(t, err)
			require.NotNil(t, response)
			require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, bytes.Repeat([]byte("x"), int(maxMessageSize+1))))
			// Frames queued before the oversize message (the hello) may still arrive; the connection must end.
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			for err == nil {
				_, _, err = conn.ReadMessage()
			}
			var closeErr *websocket.CloseError
			require.ErrorAs(t, err, &closeErr, "the server must close the connection, not let the read time out")
			require.Equal(t, websocket.CloseMessageTooBig, closeErr.Code)
			require.NoError(t, conn.Close())
			require.NoError(t, response.Body.Close())

			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.base+"/api/v1/sessions", nil)
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer "+f.token)
			restResponse, err := f.client().Do(request)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, restResponse.StatusCode)
			require.NoError(t, restResponse.Body.Close())
		})
	}
}
