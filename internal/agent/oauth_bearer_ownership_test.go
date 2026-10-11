package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/codexprovider"
	"github.com/stretchr/testify/require"
)

func oauthBearerAssertUnavailable(t *testing.T, response *http.Response, request *http.Request, id string) {
	t.Helper()
	require.NotNil(t, response)
	defer response.Body.Close()
	require.Equal(t, 401, response.StatusCode)
	require.Equal(t, "401 Unauthorized", response.Status)
	require.Same(t, request, response.Request)
	require.Equal(t, "application/json", response.Header.Get("Content-Type"))
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var got map[string]map[string]string
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, map[string]map[string]string{"error": {"message": id + " OAuth credentials unavailable: provider removed or logged out", "type": "authentication_error"}}, got)
}

func TestOAuthBearerNoAuthPassthrough(t *testing.T) {
	// Revert-check: unowned requests skip all credential and retry work.
	for _, mode := range []string{"nil", "missing", "live401", "missing401"} {
		t.Run(mode, func(t *testing.T) {
			s := oauthBearerSource()
			if mode == "missing" || mode == "missing401" {
				s.set(s.token, false)
			}
			var source codexprovider.TokenSource = s
			if mode == "nil" {
				source = nil
			}
			r := oauthBearerRequest(t)
			r.Header.Del("Authorization")
			if mode == "nil" {
				r.Header = nil
			}
			original := r.Header.Clone()
			status := 200
			if mode == "live401" || mode == "missing401" {
				status = 401
			}
			first := oauthBearerResponse(status, "untouched") //nolint:bodyclose // Fixture is closed by deferred cleanup and after reading the passthrough response.
			defer first.Body.Close()
			sends := 0
			tr := &oauthBearerTransport{source: source, base: oauthBearerRT(func(got *http.Request) (*http.Response, error) {
				sends++
				require.Same(t, r, got)
				require.Equal(t, original, got.Header)
				return first, nil
			})}
			response, err := tr.RoundTrip(r)
			require.NoError(t, err)
			require.Same(t, first, response)
			data, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			response.Body.Close()
			require.Equal(t, "untouched", string(data))
			require.Equal(t, 1, sends)
			require.Zero(t, s.lookups.Load())
			require.Zero(t, s.refreshes.Load())
		})
	}
}

func TestOAuthBearerUnavailableClient(t *testing.T) {
	// Revert-check: unavailable credentials are a JSON 401, not a network error.
	for _, mode := range []string{"nil", "missing", "blank"} {
		t.Run(mode, func(t *testing.T) {
			s := oauthBearerSource()
			s.set(s.token, false)
			if mode == "blank" {
				s.token.AccessToken = " \t"
				s.present = true
			}
			var source codexprovider.TokenSource = s
			if mode == "nil" {
				source = nil
			}
			sends := 0
			id := "provider\"\\\n"
			client := &http.Client{Transport: &oauthBearerTransport{providerID: id, source: source, base: oauthBearerRT(func(*http.Request) (*http.Response, error) {
				sends++
				return oauthBearerResponse(200, "unexpected"), nil
			})}}
			r := oauthBearerRequest(t)
			response, err := client.Do(r)
			require.NoError(t, err)
			oauthBearerAssertUnavailable(t, response, r, id)
			require.Zero(t, sends)
			require.Zero(t, s.refreshes.Load())
			if mode == "nil" {
				require.Zero(t, s.lookups.Load())
			} else {
				require.EqualValues(t, 1, s.lookups.Load())
			}
		})
	}
}
