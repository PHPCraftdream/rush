package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestAPIMatrixAuthRejectsInvalidCredentials(t *testing.T) {
	for _, ch := range apiMatrixChannels {
		t.Run(ch.name, func(t *testing.T) {
			f := newAPIMatrixFixture(t, ch)
			id, secret := matrixTokenParts(f.token)
			unknownID := "00000000"
			if unknownID == id {
				unknownID = "ffffffff"
			}
			wrongSecret := "rush_" + id + "." + strings.Repeat("A", len(secret))
			if wrongSecret == f.token {
				wrongSecret = "rush_" + id + "." + strings.Repeat("B", len(secret))
			}
			unknown := "rush_" + unknownID + "." + secret
			store := config.GlobalAPITokenStore()
			expiredAt := time.Now().Add(-time.Minute)
			expiredToken, _, err := store.CreateAPIToken("expired", []string{"read"}, "ask", &expiredAt)
			require.NoError(t, err)
			revokedToken, revokedRecord, err := store.CreateAPIToken("revoked", []string{"read"}, "ask", nil)
			require.NoError(t, err)
			require.NoError(t, store.RevokeAPIToken(revokedRecord.ID))
			cases := []struct {
				name  string
				token string
			}{
				{name: "missing"},
				{name: "malformed", token: "not-a-token"},
				{name: "unknown-id", token: unknown},
				{name: "wrong-secret", token: wrongSecret},
				{name: "expired", token: expiredToken},
				{name: "revoked", token: revokedToken},
			}
			restSuccess, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.base+"/api/v1/sessions", nil)
			require.NoError(t, err)
			restSuccess.Header.Set("Authorization", "Bearer "+f.token)
			restSuccessResp, err := f.client().Do(restSuccess)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, restSuccessResp.StatusCode)
			require.NoError(t, restSuccessResp.Body.Close())
			var unknownShape, wsUnknownShape string
			for _, tc := range cases {
				if ch.ws {
					break // the WebSocket channel keeps its failures for the upgrade cases: 10 failures per source start the back-off
				}
				t.Run(tc.name+"/rest", func(t *testing.T) {
					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.base+"/api/v1/sessions", nil)
					require.NoError(t, err)
					if tc.token != "" {
						req.Header.Set("Authorization", "Bearer "+tc.token)
					}
					resp, err := f.client().Do(req)
					require.NoError(t, err)
					defer resp.Body.Close()
					body, err := io.ReadAll(resp.Body)
					require.NoError(t, err)
					require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
					require.Equal(t, "Bearer", resp.Header.Get("WWW-Authenticate"))
					if tc.name == "unknown-id" {
						unknownShape = string(body)
					}
					if tc.name == "wrong-secret" {
						require.Equal(t, unknownShape, string(body), "unknown ID and wrong secret must not form an oracle")
					}
				})
			}
			if ch.ws {
				dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second, TLSClientConfig: f.tlsConfig(), Subprotocols: []string{"rush.v1", "rush.bearer." + f.token}}
				conn, resp, err := dialer.Dial(f.wsURL, nil)
				require.NoError(t, err)
				require.NotNil(t, resp)
				require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
				require.NoError(t, conn.Close())
				require.NoError(t, resp.Body.Close())
				for _, tc := range cases {
					t.Run(tc.name+"/ws", func(t *testing.T) {
						dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second, TLSClientConfig: f.tlsConfig()}
						if tc.token != "" {
							dialer.Subprotocols = []string{"rush.v1", "rush.bearer." + tc.token}
						}
						conn, resp, err := dialer.Dial(f.wsURL, nil)
						if conn != nil {
							_ = conn.Close()
						}
						require.Error(t, err)
						require.NotNil(t, resp)
						defer resp.Body.Close()
						body, readErr := io.ReadAll(resp.Body)
						require.NoError(t, readErr)
						require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
						require.Equal(t, "Bearer", resp.Header.Get("WWW-Authenticate"))
						if tc.name == "unknown-id" {
							wsUnknownShape = string(body)
						}
						if tc.name == "wrong-secret" {
							require.Equal(t, wsUnknownShape, string(body), "unknown ID and wrong secret must not form a WebSocket oracle")
						}
					})
				}
			}
		})
	}
}

func TestAPIMatrixAuthRejectsQueryCredential(t *testing.T) {
	for _, ch := range apiMatrixChannels {
		t.Run(ch.name, func(t *testing.T) {
			f := newAPIMatrixFixture(t, ch)
			if !ch.ws {
				control, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.base+"/api/v1/sessions", nil)
				require.NoError(t, err)
				control.Header.Set("Authorization", "Bearer "+f.token)
				controlResp, err := f.client().Do(control)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, controlResp.StatusCode)
				require.NoError(t, controlResp.Body.Close())
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.base+"/api/v1/sessions?token="+f.token, nil)
				require.NoError(t, err)
				resp, err := f.client().Do(req)
				require.NoError(t, err)
				defer resp.Body.Close()
				require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
				require.Equal(t, "Bearer", resp.Header.Get("WWW-Authenticate"))
				return
			}
			dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second, TLSClientConfig: f.tlsConfig(), Subprotocols: []string{"rush.v1", "rush.bearer." + f.token}}
			controlConn, controlResp, err := dialer.Dial(f.wsURL, nil)
			require.NoError(t, err)
			require.NotNil(t, controlResp)
			require.Equal(t, http.StatusSwitchingProtocols, controlResp.StatusCode)
			require.NoError(t, controlConn.Close())
			require.NoError(t, controlResp.Body.Close())
			dialer = websocket.Dialer{HandshakeTimeout: 3 * time.Second, TLSClientConfig: f.tlsConfig()}
			conn, resp, err := dialer.Dial(f.wsURL+"?token="+f.token, nil)
			if conn != nil {
				_ = conn.Close()
			}
			require.Error(t, err)
			require.NotNil(t, resp)
			defer resp.Body.Close()
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			require.Equal(t, "Bearer", resp.Header.Get("WWW-Authenticate"))
		})
	}
}
