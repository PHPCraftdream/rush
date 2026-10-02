package codex

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestTokenExchangeFailureDiagnosticsRedactSecrets(t *testing.T) {
	const secret = "sensitive-refresh-token-not-for-logs"
	cases := []struct {
		name         string
		contentType  string
		body         string
		refreshToken string
		ray          string
		requestID    string
		want         []string
		unwanted     []string
	}{
		{
			name:        "structured OAuth error",
			contentType: "application/json; charset=utf-8",
			body:        `{"error":"invalid_grant","error_description":"` + secret + `"}`,
			ray:         "1234567890abcdef-FRA",
			requestID:   "req_1234567890abcdef",
			want:        []string{"kind=json", "content_type=application/json", "oauth_error=invalid_grant", "cf_ray=1234567890abcdef-FRA", "request_id=req_1234567890abcdef"},
		},
		{
			name:        "HTML gateway block",
			contentType: "text/html; charset=utf-8",
			body:        "<!doctype html><html>" + secret + "</html>",
			ray:         secret,
			requestID:   secret,
			want:        []string{"kind=html", "content_type=text/html"},
			unwanted:    []string{"cf_ray=", "request_id="},
		},
		{
			name:         "hex credential echoed as ray ID",
			refreshToken: "1234567890abcdef",
			ray:          "1234567890abcdef",
			want:         []string{"kind=empty"},
			unwanted:     []string{"cf_ray="},
		},
		{
			name:         "credential echoed as request ID",
			refreshToken: "req_1234567890abcdef",
			requestID:    "req_1234567890abcdef",
			want:         []string{"kind=empty"},
			unwanted:     []string{"request_id="},
		},
		{
			name: "empty response",
			want: []string{"kind=empty", "bytes=0", "content_type=missing"},
		},
		{
			name:        "nested OAuth error",
			contentType: "application/json",
			body:        `{"error":{"code":"invalid_client","message":"` + secret + `"}}`,
			want:        []string{"kind=json", "oauth_error=invalid_client"},
		},
		{
			name:        "unknown structured error",
			contentType: "application/json",
			body:        `{"error":"` + secret + `","error_description":"` + secret + `"}`,
			want:        []string{"kind=json", "content_type=application/json"},
			unwanted:    []string{"oauth_error="},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			original := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
			t.Cleanup(func() { slog.SetDefault(original) })

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				if tc.ray != "" {
					w.Header().Set("Cf-Ray", tc.ray)
				}
				if tc.requestID != "" {
					w.Header().Set("X-Request-Id", tc.requestID)
				}
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)

			refreshToken := tc.refreshToken
			if refreshToken == "" {
				refreshToken = secret
			}
			_, err := refresh(context.Background(), server.Client(), server.URL, refreshToken)
			var exchange *oauth.TokenExchangeError
			require.ErrorAs(t, err, &exchange)
			require.Equal(t, http.StatusForbidden, exchange.StatusCode)
			for _, expected := range tc.want {
				require.Contains(t, err.Error(), expected)
				require.Contains(t, output.String(), expected)
			}
			for _, forbidden := range append(tc.unwanted, secret, refreshToken, "<html>", "error_description") {
				require.NotContains(t, err.Error(), forbidden)
				require.NotContains(t, output.String(), forbidden)
			}
			if strings.Contains(tc.body, "invalid_grant") {
				require.True(t, exchange.IsRefreshTokenRevoked())
			}
		})
	}
}
