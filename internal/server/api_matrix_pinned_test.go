package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func newPinnedTLSMatrix(t *testing.T) (*pinFixture, *tls.Config) {
	t.Helper()
	isolateAPIGlobals(t)
	workspace := t.TempDir()
	app := newAttachmentsTestApp(t, workspace, t.TempDir())
	own, err := app.Sessions.Create(t.Context(), "pinned")
	require.NoError(t, err)
	foreign, err := app.Sessions.Create(t.Context(), "foreign")
	require.NoError(t, err)
	store := config.GlobalAPITokenStore()
	pinned, _, err := store.CreateAPITokenWithBinding("pinned", []string{"read", "run"}, "ask", nil, config.APITokenBinding{Kind: "session", Pin: &config.APITokenPinBinding{Session: own.ID, Workspace: workspace}})
	require.NoError(t, err)
	control, _, err := store.CreateAPIToken("control", []string{"read", "run"}, "ask", nil)
	require.NoError(t, err)
	cert, key, certificate := writeTLSTestPair(t, t.TempDir(), "pinned-matrix", time.Now(), []string{"localhost", "127.0.0.1"})
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	tlsConfig := &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
	policy := ListenerPolicy{Address: "127.0.0.1:0", Workspace: workspace, Surfaces: []ListenerSurface{SurfaceREST, SurfaceWS}, TLS: ListenerTLS{Cert: cert, Key: key}, Limits: ListenerLimits{MaxConnsPerToken: 20, RPS: 100000, Burst: 100000}}
	api, err := NewServeAPI(policy, app, store, nil)
	require.NoError(t, err)
	api.Configure(&policy)
	srv := NewWithPolicy(app, policy, nil)
	base := "https" + strings.TrimPrefix(startPolicyServer(t, srv), "http")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, api.Stop(ctx))
	})
	return &pinFixture{base: base, endpoint: "wss" + strings.TrimPrefix(base, "https") + "/ws", app: app, server: srv, own: own, other: foreign, pinned: pinned, control: control}, tlsConfig
}

func TestPinnedTLSRESTConfinementThroughProductionListener(t *testing.T) {
	f, tlsConfig := newPinnedTLSMatrix(t)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}, Timeout: 5 * time.Second}
	request := func(token, path string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.base+"/api/v1"+path, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(body)
	}
	status, body := request(f.pinned, "/sessions")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{f.own.ID}, pageSessionIDs(t, body))
	status, _ = request(f.pinned, "/sessions/"+f.own.ID)
	require.Equal(t, http.StatusOK, status)
	foreignStatus, foreignBody := request(f.pinned, "/sessions/"+f.other.ID)
	missingStatus, missingBody := request(f.pinned, "/sessions/no-such-session")
	require.Equal(t, http.StatusNotFound, foreignStatus)
	require.Equal(t, http.StatusNotFound, missingStatus)
	require.Equal(t, problemShape(t, missingBody), problemShape(t, foreignBody))
	status, body = request(f.control, "/sessions")
	require.Equal(t, http.StatusOK, status)
	require.ElementsMatch(t, []string{f.own.ID, f.other.ID}, pageSessionIDs(t, body))
}

func TestPinnedTLSWSConfinementThroughProductionListener(t *testing.T) {
	f, tlsConfig := newPinnedTLSMatrix(t)
	dial := func(token string) *websocket.Conn {
		t.Helper()
		d := websocket.Dialer{HandshakeTimeout: 3 * time.Second, TLSClientConfig: tlsConfig, Subprotocols: []string{"rush.v1", "rush.bearer." + token}}
		conn, resp, err := d.Dial(f.endpoint, nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	pinned := dial(f.pinned)
	control := dial(f.control)
	list := func(conn *websocket.Conn) []session.Session {
		t.Helper()
		reply, _ := exchange(t, conn, WSMessage{ID: "list", Type: CmdListSessions})
		require.Equal(t, EventSessionsList, reply.Type)
		var sessions []session.Session
		require.NoError(t, json.Unmarshal(reply.Payload, &sessions))
		return sessions
	}
	require.Len(t, list(pinned), 1)
	require.Equal(t, f.own.ID, list(pinned)[0].ID)
	require.ElementsMatch(t, []string{f.own.ID, f.other.ID}, []string{list(control)[0].ID, list(control)[1].ID})
	payload := func(id string) json.RawMessage {
		raw, _ := json.Marshal(map[string]string{"sessionID": id})
		return raw
	}
	foreignReply, _ := exchange(t, pinned, WSMessage{ID: "foreign", Type: CmdLoadMessages, Payload: payload(f.other.ID)})
	missingReply, _ := exchange(t, pinned, WSMessage{ID: "missing", Type: CmdLoadMessages, Payload: payload("no-such-session")})
	require.Equal(t, EventError, foreignReply.Type)
	require.Equal(t, EventError, missingReply.Type)
	require.Equal(t, foreignReply.Error, missingReply.Error)
	ownReply, _ := exchange(t, pinned, WSMessage{ID: "own", Type: CmdLoadMessages, Payload: payload(f.own.ID)})
	require.Equal(t, EventMessagesList, ownReply.Type)
}
