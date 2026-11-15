package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestAPIMatrixScopeAndModePerChannel(t *testing.T) {
	for _, ch := range apiMatrixChannels {
		t.Run(ch.name, func(t *testing.T) {
			f := newAPIMatrixFixture(t, ch)
			store := config.GlobalAPITokenStore()
			readToken, _, err := store.CreateAPIToken("read", []string{"read"}, "ask", nil)
			require.NoError(t, err)
			runToken, _, err := store.CreateAPIToken("run", []string{"run"}, "ask", nil)
			require.NoError(t, err)
			if ch.ws {
				dial := func(token string) *websocket.Conn {
					d := websocket.Dialer{TLSClientConfig: f.tlsConfig(), Subprotocols: []string{"rush.v1", "rush.bearer." + token}}
					conn, resp, dialErr := d.Dial(f.wsURL, nil)
					require.NoError(t, dialErr)
					require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
					_ = resp.Body.Close()
					t.Cleanup(func() { _ = conn.Close() })
					return conn
				}
				readConn, runConn := dial(readToken), dial(runToken)
				readReply, _ := exchange(t, readConn, WSMessage{ID: "denied", Type: CmdCreateSession, Payload: json.RawMessage(`{"title":"forbidden"}`)})
				require.Equal(t, EventError, readReply.Type)
				require.Equal(t, wsDeniedScope, readReply.Error)
				listReply, _ := exchange(t, readConn, WSMessage{ID: "read", Type: CmdListSessions})
				require.Equal(t, EventSessionsList, listReply.Type)
				sendCommand(t, runConn, WSMessage{ID: "created", Type: CmdCreateSession, Payload: json.RawMessage(`{"title":"scope control"}`)})
				awaitType(t, runConn, EventSessionCreated)
				return
			}
			request := func(token, method string) int {
				req, requestErr := http.NewRequestWithContext(t.Context(), method, f.base+"/api/v1/sessions", nil)
				require.NoError(t, requestErr)
				req.Header.Set("Authorization", "Bearer "+token)
				resp, requestErr := f.client().Do(req)
				require.NoError(t, requestErr)
				defer resp.Body.Close()
				return resp.StatusCode
			}
			require.Equal(t, http.StatusOK, request(readToken, http.MethodGet))
			require.Equal(t, http.StatusForbidden, request(readToken, http.MethodPost))
			require.Equal(t, http.StatusOK, request(f.token, http.MethodGet))
			require.Contains(t, []int{http.StatusCreated, http.StatusOK}, request(runToken, http.MethodPost))
		})
	}
}

func TestAPIMatrixAutoPermissionRequiresAuthorizedTokenCreation(t *testing.T) {
	isolateAPIGlobals(t)
	store := config.GlobalAPITokenStore()
	_, _, err := store.CreateAPIToken("unsafe-auto", []string{"run"}, "auto", nil)
	require.Error(t, err)
	admin, _, err := store.CreateAPIToken("admin-auto", []string{"admin"}, "auto", nil)
	require.NoError(t, err)
	require.NotEmpty(t, admin)
}
