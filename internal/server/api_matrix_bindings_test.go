package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// bindingMatrix is one production listener (NewServeAPI + Configure +
// NewWithPolicy) on one of the four channels, with a token of every binding kind.
type bindingMatrix struct {
	channel   channel
	base      string
	wsURL     string
	tlsConfig *tls.Config
	none      string
	pinned    string
	batch     string
	session   string
}

func newBindingMatrix(t *testing.T, ch channel, route string) *bindingMatrix {
	t.Helper()
	isolateAPIGlobals(t)
	workspace := t.TempDir()
	app := newAttachmentsTestApp(t, workspace, t.TempDir())
	own, err := app.Sessions.Create(t.Context(), "pinned")
	require.NoError(t, err)
	store := config.GlobalAPITokenStore()
	create := func(name string, binding config.APITokenBinding) string {
		token, _, err := store.CreateAPITokenWithBinding(name, []string{"read", "run"}, "ask", nil, binding)
		require.NoError(t, err)
		return token
	}
	m := &bindingMatrix{
		channel: ch, session: own.ID,
		none:   create("none", config.APITokenBinding{}),
		pinned: create("pinned", config.APITokenBinding{Kind: "session", Pin: &config.APITokenPinBinding{Session: own.ID, Workspace: workspace}}),
		batch:  create("batch", config.APITokenBinding{Kind: "batch", Batch: &config.APITokenBatchBinding{MaxRoots: 2, MaxParallel: 2}}),
	}
	policy := ListenerPolicy{
		Address: "127.0.0.1:0", Workspace: workspace, Route: route,
		Surfaces: []ListenerSurface{SurfaceREST, SurfaceWS},
		Limits:   ListenerLimits{MaxConnsPerToken: 20, RPS: 100000, Burst: 100000},
	}
	if ch.tls {
		cert, key, certificate := writeTLSTestPair(t, t.TempDir(), "binding-matrix", time.Now(), []string{"localhost", "127.0.0.1"})
		policy.TLS = ListenerTLS{Cert: cert, Key: key}
		roots := x509.NewCertPool()
		roots.AddCert(certificate)
		m.tlsConfig = &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
	}
	api, err := NewServeAPI(policy, app, store, nil)
	require.NoError(t, err)
	api.Configure(&policy)
	m.base = startPolicyServer(t, NewWithPolicy(app, policy, nil))
	if ch.tls {
		m.base = "https" + strings.TrimPrefix(m.base, "http")
	}
	m.wsURL = "ws" + strings.TrimPrefix(m.base, "http") + "/ws"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, api.Stop(ctx))
	})
	return m
}

func (m *bindingMatrix) rest(t *testing.T, token, method, path, body string) (int, string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: m.tlsConfig}, Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(t.Context(), method, m.base+"/api/v1"+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw)
}

func (m *bindingMatrix) dial(t *testing.T, token string) *websocket.Conn {
	t.Helper()
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second, TLSClientConfig: m.tlsConfig, Subprotocols: []string{"rush.v1", "rush.bearer." + token}}
	conn, resp, err := dialer.Dial(m.wsURL, nil)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Contains(t, string(raw), EventHello)
	return conn
}

// TestBindingMatrix drives every binding kind through each of the four
// channels. The binding rule must hold on the transport the client picked, and
// a token of another kind on the same listener must stay unaffected.
func TestBindingMatrix(t *testing.T) {
	type expectation struct {
		name                string
		token               func(*bindingMatrix) string
		route               string
		createStatus        int
		denial              string // body or WS error text on refusal
		intakeForbiddenText string // "" = batch intake is not forbidden
	}
	expectations := []expectation{
		{name: "unbound", token: func(m *bindingMatrix) string { return m.none }, createStatus: http.StatusCreated},
		{name: "session-pinned", token: func(m *bindingMatrix) string { return m.pinned }, createStatus: http.StatusForbidden, denial: "pinned-token", intakeForbiddenText: "pinned-token"},
		{name: "batch-bound", token: func(m *bindingMatrix) string { return m.batch }, createStatus: http.StatusForbidden, denial: "batch-only"},
		{name: "unbound-on-route-batch", token: func(m *bindingMatrix) string { return m.none }, route: routeBatch, createStatus: http.StatusForbidden, denial: "batch-only"},
	}
	for _, ch := range apiMatrixChannels {
		for _, tc := range expectations {
			t.Run(ch.name+"/"+tc.name, func(t *testing.T) {
				m := newBindingMatrix(t, ch, tc.route)
				token := tc.token(m)
				if ch.ws {
					conn := m.dial(t, token)
					if tc.denial == "" {
						sendCommand(t, conn, WSMessage{ID: "create", Type: CmdCreateSession, Payload: []byte(`{"title":"x"}`)})
						awaitType(t, conn, EventSessionCreated)
					} else {
						reply, _ := exchange(t, conn, WSMessage{ID: "create", Type: CmdCreateSession, Payload: []byte(`{"title":"x"}`)})
						require.Equal(t, EventError, reply.Type)
						require.Equal(t, "forbidden: "+tc.denial, reply.Error)
					}
					reply, _ := exchange(t, conn, WSMessage{ID: "list", Type: CmdListSessions})
					require.Equal(t, EventSessionsList, reply.Type, "reads stay available under every binding")
					return
				}
				status, body := m.rest(t, token, http.MethodPost, "/sessions", `{"title":"x"}`)
				require.Equal(t, tc.createStatus, status, body)
				if tc.denial != "" {
					require.Contains(t, body, tc.denial)
				}
				for _, path := range []string{"/health", "/sessions", "/sessions/" + m.session, "/sessions/" + m.session + "/messages"} {
					status, body = m.rest(t, token, http.MethodGet, path, "")
					require.Equal(t, http.StatusOK, status, "reads stay available under every binding (%s): %s", path, body)
				}
				if tc.name != "unbound" {
					status, body = m.rest(t, token, http.MethodPost, "/sessions/"+m.session+"/runs", `{"prompt":"x"}`)
					if tc.name == "session-pinned" {
						require.NotEqual(t, http.StatusUnauthorized, status, "the pinned token may work on its own session: %s", body)
					} else {
						require.Equal(t, http.StatusForbidden, status, body)
						require.Contains(t, body, "batch-only")
					}
				}
				status, body = m.rest(t, token, http.MethodGet, "/batches/no-such-root", "")
				if tc.intakeForbiddenText != "" {
					require.Equal(t, http.StatusForbidden, status, body)
					require.Contains(t, body, tc.intakeForbiddenText)
				} else {
					require.NotEqual(t, http.StatusForbidden, status, "batch routes are not refused by the binding: %s", body)
				}
			})
		}
	}
}
