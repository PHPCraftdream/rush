package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent/codexprovider"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

type limitTestTransport func(*http.Request) (*http.Response, error)

func (f limitTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func limitTestClient(server *httptest.Server) *http.Client {
	target, _ := url.Parse(server.URL)
	return &http.Client{Transport: limitTestTransport(func(r *http.Request) (*http.Response, error) {
		forwarded := r.Clone(r.Context())
		copyURL := *r.URL
		forwarded.URL = &copyURL
		forwarded.URL.Scheme = target.Scheme
		forwarded.URL.Host = target.Host
		forwarded.Host = target.Host
		return server.Client().Transport.RoundTrip(forwarded)
	})}
}

// Revert-check: limitErrorObject and limitProvider must retain compat identity and business codes.
func TestProviderLimit1282CompatHTTP(t *testing.T) {
	// https://docs.z.ai/api-reference/api-code; not reproduced locally.
	// https://platform.stepfun.ai/docs/en/api-reference/error-codes; not reproduced locally.
	for _, tc := range []struct {
		name, url, body string
		status          int
		hard            bool
	}{
		{"zai", "https://api.z.ai/api/paas/v4", `{"error":{"code":"1113","message":"Insufficient balance or no resource package. Please recharge."}}`, 429, true},
		{"zai numeric", "https://api.z.ai/api/paas/v4", `{"error":{"code":1309,"message":"Your GLM Coding Plan package has expired"}}`, 429, true},
		{"zai transient", "https://api.z.ai/api/paas/v4", `{"error":{"code":"1302","message":"Rate limit reached for requests"}}`, 429, false},
		{"step", "https://api.stepfun.ai/v1", `{"error":{"message":"Insufficient balance"}}`, 402, true},
		{"step transient", "https://api.stepfun.ai/v1", `{"error":{"message":"quota exceeded"}}`, 429, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(server.Close)
			provider, err := openaicompat.New(openaicompat.WithBaseURL(tc.url), openaicompat.WithAPIKey("synthetic"), openaicompat.WithHTTPClient(limitTestClient(server)))
			require.NoError(t, err)
			model, err := provider.LanguageModel(t.Context(), "synthetic")
			require.NoError(t, err)
			_, err = model.Generate(t.Context(), fantasy.Call{})
			var pe *fantasy.ProviderError
			require.ErrorAs(t, err, &pe)
			require.Equal(t, tc.url+"/chat/completions", pe.URL)
			require.Contains(t, string(pe.ResponseBody), "HTTP/1.1")
			require.Contains(t, string(pe.ResponseBody), tc.body)
			require.Equal(t, "1", pe.ResponseHeaders["Retry-After"])
			require.Equal(t, tc.hard, IsHardQuotaLimit(err))
			t.Logf("URL=%s body=%q headers=%v", pe.URL, pe.ResponseBody, pe.ResponseHeaders)
		})
	}
}

// Revert-check: codexStreamFailure/providerError must retain type, raw body and reset metadata.
func TestProviderLimit1282CodexHTTPAndSSE(t *testing.T) {
	// https://github.com/acmiyaguchi/fen/issues/583; not reproduced locally.
	for _, mode := range []string{"http", "error", "response.failed"} {
		t.Run(mode, func(t *testing.T) {
			body := `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":1800}}`
			if mode == "error" {
				body = `{"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":1800}}`
			}
			if mode == "response.failed" {
				body = `{"type":"response.failed","response":{"error":{"code":"usage_limit_reached","message":"The usage limit has been reached","resets_at":1900000000}}}`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if mode == "http" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(429)
					_, _ = fmt.Fprint(w, body)
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
				}
			}))
			t.Cleanup(server.Close)
			provider, err := codexprovider.New(limitTestClient(server), &oauth.Token{AccessToken: "synthetic"})
			require.NoError(t, err)
			model, err := provider.LanguageModel(t.Context(), "gpt-5-codex")
			require.NoError(t, err)
			_, err = model.Generate(t.Context(), fantasy.Call{})
			var pe *fantasy.ProviderError
			require.ErrorAs(t, err, &pe)
			require.Equal(t, body, string(pe.ResponseBody))
			require.Equal(t, "usage_limit_reached: The usage limit has been reached", pe.Message)
			if mode != "http" {
				require.Zero(t, pe.StatusCode)
				require.False(t, pe.IsRetryable())
			}
			require.True(t, IsHardQuotaLimit(err))
			require.Equal(t, classTerminal, classifyProviderError(err))
			now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
			reset, ok := QuotaLimitResetTime(err, now)
			require.True(t, ok)
			if mode == "response.failed" {
				require.Equal(t, time.Unix(1900000000, 0), reset)
			} else {
				require.Equal(t, now.Add(30*time.Minute), reset)
			}
		})
	}
}
