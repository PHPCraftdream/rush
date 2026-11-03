package server

import (
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestAPIMatrixServeListenerNeverFallsBackToUI(t *testing.T) {
	for _, ch := range apiMatrixChannels {
		t.Run(ch.name, func(t *testing.T) {
			f := newAPIMatrixFixture(t, ch)
			client := f.client()
			for _, path := range []string{"/", "/auth", "/index.html", "/%2e%2e/index.html", "//index.html", "/api/v1/sessions/"} {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.base+path, nil)
				require.NoError(t, err)
				resp, err := client.Do(req)
				require.NoError(t, err)
				_, readErr := io.Copy(io.Discard, resp.Body)
				require.NoError(t, readErr)
				require.NoError(t, resp.Body.Close())
				require.Contains(t, []int{http.StatusUnauthorized, http.StatusNotFound}, resp.StatusCode, "%s must be refused, never served or redirected to a UI", path)
				if path == "/api/v1/sessions/" {
					require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "trailing API path must not bypass auth")
				}
			}
			// A normal authenticated API request proves that the listener remains live.
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.base+"/api/v1/sessions", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+f.token)
			resp, err := client.Do(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.NoError(t, resp.Body.Close())

			if ch.ws {
				for _, credential := range []string{"cookie", "query"} {
					dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second, TLSClientConfig: f.tlsConfig()}
					target := f.wsURL
					header := http.Header{}
					if credential == "cookie" {
						header.Set("Cookie", cookieName+"="+url.QueryEscape(f.token))
					} else {
						target += "?token=" + url.QueryEscape(f.token)
					}
					conn, wsResp, dialErr := dialer.Dial(target, header)
					if conn != nil {
						_ = conn.Close()
					}
					require.Error(t, dialErr)
					require.NotNil(t, wsResp)
					require.Equal(t, http.StatusUnauthorized, wsResp.StatusCode)
					require.NoError(t, wsResp.Body.Close())
				}
			}
		})
	}
}
