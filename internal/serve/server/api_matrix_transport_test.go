package server

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestAPIMatrixBruteForceBackoffIsSourceScoped(t *testing.T) {
	isolateAPIGlobals(t)
	f := newAPIMatrixFixture(t, apiMatrixChannels[0])
	clock := time.Unix(1_800_000_000, 0)
	auth := Chain(ChainConfig{
		AuthConfig: AuthConfig{Verifier: config.GlobalAPITokenStore(), Clock: func() time.Time { return clock }},
		Logger:     slog.New(slog.NewTextHandler(&syncBuffer{}, nil)), RPS: 100000, Burst: 100000,
	})
	handler := auth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	request := func(remote, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/version", nil)
		req.RemoteAddr = remote
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	for range 10 {
		require.Equal(t, http.StatusUnauthorized, request("192.0.2.1:1234", "not-a-token").Code)
	}
	require.Equal(t, http.StatusTooManyRequests, request("192.0.2.1:1234", f.token).Code)
	require.Equal(t, http.StatusOK, request("192.0.2.2:1234", f.token).Code)
	clock = clock.Add(2 * time.Second)
	require.Equal(t, http.StatusOK, request("192.0.2.1:1234", f.token).Code)
}

func TestAPIMatrixTokenQueryRejectedAndRedacted(t *testing.T) {
	for _, ch := range apiMatrixChannels {
		t.Run(ch.name, func(t *testing.T) {
			var logs syncBuffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			f := newAPIMatrixFixture(t, ch)
			readToken, _, err := config.GlobalAPITokenStore().CreateAPIToken("redaction-read", []string{"read"}, "ask", nil)
			require.NoError(t, err)
			request := func(method, path string, body io.Reader, token string) *http.Response {
				t.Helper()
				req, err := http.NewRequestWithContext(t.Context(), method, f.base+path, body)
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err := f.client().Do(req)
				require.NoError(t, err)
				return resp
			}
			for _, tc := range []struct {
				method string
				path   string
				body   io.Reader
				status int
				token  string
			}{
				{method: http.MethodGet, path: "/api/v1/sessions", status: http.StatusOK, token: f.token},
				{method: http.MethodPost, path: "/api/v1/sessions", status: http.StatusForbidden, token: readToken},
				{method: http.MethodGet, path: "/api/v1/sessions/no-such-session", status: http.StatusNotFound, token: f.token},
				{method: http.MethodPost, path: "/api/v1/runs", body: strings.NewReader(strings.Repeat("x", 65)), status: http.StatusRequestEntityTooLarge, token: f.token},
			} {
				response := request(tc.method, tc.path, tc.body, tc.token)
				require.Equal(t, tc.status, response.StatusCode)
				require.NoError(t, response.Body.Close())
			}
			if ch.ws {
				wsDialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second, TLSClientConfig: f.tlsConfig(), Subprotocols: []string{"rush.v1", "rush.bearer." + f.token}}
				wsConn, wsResponse, err := wsDialer.Dial(f.wsURL, nil)
				require.NoError(t, err)
				require.Equal(t, http.StatusSwitchingProtocols, wsResponse.StatusCode)
				require.NoError(t, wsConn.Close())
				require.NoError(t, wsResponse.Body.Close())
				wsDialer = websocket.Dialer{HandshakeTimeout: 3 * time.Second, TLSClientConfig: f.tlsConfig(), Subprotocols: []string{"rush.v1", "rush.bearer." + readToken}}
				wsConn, wsResponse, err = wsDialer.Dial(f.wsURL, nil)
				require.NoError(t, err)
				denied, _ := exchange(t, wsConn, WSMessage{ID: "scope-denied", Type: CmdCreateSession})
				require.Equal(t, EventError, denied.Type)
				require.Equal(t, wsDeniedScope, denied.Error)
				require.NoError(t, wsConn.Close())
				require.NoError(t, wsResponse.Body.Close())
			}
			restQuery, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.base+"/api/v1/sessions?token="+f.token, nil)
			require.NoError(t, err)
			restQueryResponse, err := f.client().Do(restQuery)
			require.NoError(t, err)
			require.Equal(t, http.StatusUnauthorized, restQueryResponse.StatusCode)
			require.NoError(t, restQueryResponse.Body.Close())
			if ch.ws {
				dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second, TLSClientConfig: f.tlsConfig()}
				conn, response, err := dialer.Dial(f.wsURL+"?token="+f.token, nil)
				if conn != nil {
					_ = conn.Close()
				}
				require.Error(t, err)
				require.NotNil(t, response)
				require.Equal(t, http.StatusUnauthorized, response.StatusCode)
				require.NoError(t, response.Body.Close())
			}
			_, secret := matrixTokenParts(f.token)
			for _, forbidden := range []string{f.token, secret, "?token=" + f.token, "Bearer " + f.token} {
				require.NotContains(t, logs.String(), forbidden)
			}
		})
	}
}

func TestAPIMatrixTLSOfferAndDowngradeRules(t *testing.T) {
	for _, ch := range apiMatrixChannels {
		if !ch.tls {
			continue
		}
		t.Run(ch.name, func(t *testing.T) {
			f := newAPIMatrixFixture(t, ch)
			tlsConfig := f.tlsConfig()
			tlsConfig.NextProtos = []string{"h2", "http/1.1"}
			transport := &http.Transport{TLSClientConfig: tlsConfig}
			defer transport.CloseIdleConnections()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.base+"/api/v1/sessions", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+f.token)
			resp, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Do(req)
			require.NoError(t, err)
			require.Equal(t, "HTTP/1.1", resp.Proto)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.NoError(t, resp.Body.Close())
			if ch.ws {
				dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second, TLSClientConfig: f.tlsConfig(), Subprotocols: []string{"rush.v1", "rush.bearer." + f.token}}
				conn, wsResponse, dialErr := dialer.Dial(f.wsURL, nil)
				require.NoError(t, dialErr)
				require.NotNil(t, wsResponse)
				require.Equal(t, http.StatusSwitchingProtocols, wsResponse.StatusCode)
				require.NoError(t, conn.Close())
				require.NoError(t, wsResponse.Body.Close())
			}
			address := strings.TrimPrefix(f.base, "https://")
			alpnConfig := f.tlsConfig()
			alpnConfig.NextProtos = []string{"h2", "http/1.1"}
			alpnConn, alpnErr := (&tls.Dialer{Config: alpnConfig}).DialContext(t.Context(), "tcp", address)
			require.NoError(t, alpnErr)
			require.Equal(t, "http/1.1", alpnConn.(*tls.Conn).ConnectionState().NegotiatedProtocol, "a client offering h2 must still negotiate http/1.1")
			require.NoError(t, alpnConn.Close())
			conn, err := (&tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}}).DialContext(t.Context(), "tcp", address)
			require.Error(t, err)
			if conn != nil {
				_ = conn.Close()
			}
			plainRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+"/api/v1/sessions", nil)
			require.NoError(t, err)
			plainResponse, plainErr := (&http.Client{Timeout: time.Second}).Do(plainRequest)
			if plainErr == nil {
				_ = plainResponse.Body.Close()
				require.NotEqual(t, http.StatusOK, plainResponse.StatusCode)
			}
		})
	}
}

func TestAPIMatrixCleartextPolicyRequiresExplicitOptIn(t *testing.T) {
	for _, allow := range []bool{false, true} {
		policy := ListenerPolicy{Address: "192.0.2.10:8080", Surfaces: []ListenerSurface{SurfaceREST}, Auth: DenyAllAuth, AllowCleartext: allow}
		err := policy.Validate()
		if allow {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
			require.Contains(t, err.Error(), "non-loopback address requires TLS or allow_cleartext")
		}
	}
}
