package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestOAuthBearerRedirect(t *testing.T) {
	// Revert-check: never restore auth stripped on cross-host redirects.
	for _, sameHost := range []bool{false, true} {
		name := "cross-host"
		if sameHost {
			name = "same-host"
		}
		t.Run(name, func(t *testing.T) {
			s := oauthBearerSource()
			s.set(oauth.Token{AccessToken: "live"}, true)
			var oauthAAuth, oauthBAuth atomic.Value
			var oauthASends, oauthBSends atomic.Int32
			b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				oauthBSends.Add(1)
				oauthBAuth.Store(r.Header.Get("Authorization"))
				_, _ = io.WriteString(w, "ok")
			}))
			defer b.Close()
			a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				oauthASends.Add(1)
				oauthAAuth.Store(r.Header.Get("Authorization"))
				s.set(oauth.Token{AccessToken: "rotated"}, true)
				http.Redirect(w, r, b.URL, http.StatusFound)
			}))
			defer a.Close()
			url := a.URL
			if !sameHost {
				url = strings.Replace(url, "127.0.0.1", "localhost", 1)
			}
			base := &http.Transport{Proxy: nil}
			defer base.CloseIdleConnections()
			client := &http.Client{Timeout: 3 * time.Second, Transport: &oauthBearerTransport{base: base, source: s}}
			r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
			require.NoError(t, err)
			r.Header.Set("Authorization", "Bearer baked")
			response, err := client.Do(r)
			require.NoError(t, err)
			response.Body.Close()
			require.EqualValues(t, 1, oauthASends.Load())
			require.EqualValues(t, 1, oauthBSends.Load())
			require.Equal(t, "Bearer live", oauthAAuth.Load())
			if sameHost {
				require.Equal(t, "Bearer rotated", oauthBAuth.Load())
			} else {
				require.Equal(t, "", oauthBAuth.Load())
			}
			require.Zero(t, s.refreshes.Load())
		})
	}
}
