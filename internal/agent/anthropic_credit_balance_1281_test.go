package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"github.com/stretchr/testify/require"
)

// Revert-check: Pins cli_active_work.go:22-24's captured billing recognition.
func TestAnthropicCreditBalance1281(t *testing.T) {
	body, err := os.ReadFile("testdata/anthropic_credit_balance_1281.json")
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/messages", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_REDACTED")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	provider, err := anthropic.New(anthropic.WithAPIKey("local-probe-only"), anthropic.WithBaseURL(server.URL), anthropic.WithHTTPClient(server.Client()))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	model, err := provider.LanguageModel(ctx, "claude-sonnet-4-20250514")
	require.NoError(t, err)
	for _, method := range []string{"generate", "stream"} {
		t.Run(method, func(t *testing.T) {
			call := fantasy.Call{Prompt: []fantasy.Message{fantasy.NewUserMessage("local probe")}}
			var callErr error
			if method == "generate" {
				_, callErr = model.Generate(ctx, call)
			} else {
				var stream fantasy.StreamResponse
				stream, callErr = model.Stream(ctx, call)
				if callErr == nil {
					for part := range stream {
						if part.Type == fantasy.StreamPartTypeError {
							callErr = part.Error
						}
					}
				}
			}
			var pe *fantasy.ProviderError
			require.ErrorAs(t, callErr, &pe)
			require.Equal(t, http.StatusBadRequest, pe.StatusCode)
			require.Equal(t, "bad request", pe.Title)
			require.Equal(t, fmt.Sprintf("POST %q: 400 Bad Request (Request-ID: req_REDACTED) %s", server.URL+"/v1/messages", body), pe.Message)
			require.Contains(t, pe.Message, "Your credit balance is too low to access the Anthropic API.")
			require.Contains(t, pe.Message, "req_REDACTED")
			require.Equal(t, classTerminal, classifyProviderError(callErr))
			for _, wrappedErr := range []error{
				callErr,
				&fantasy.RetryError{Errors: []error{callErr}},
				&ErrCallAlreadyAttempted{Err: callErr},
				&ErrCallAlreadyAttempted{Err: &fantasy.RetryError{Errors: []error{callErr}}},
			} {
				var reached *fantasy.ProviderError
				require.ErrorAs(t, wrappedErr, &reached)
				require.Same(t, pe, reached)
				require.True(t, IsHardQuotaLimit(wrappedErr))
			}
			t.Logf("StatusCode=%d Title=%q Message=%q", pe.StatusCode, pe.Title, pe.Message)
			wrapped := fmt.Errorf("probe wrapper: %w", callErr)
			reset, found := QuotaLimitResetTime(wrapped, time.Now())
			guidance, guided := QuotaLimitGuidance(wrapped)
			t.Logf("quota=%v hard=%v rateWait=%v reset=%v/%v guidance=%q/%v", isQuotaLimit(pe), IsHardQuotaLimit(wrapped), isRateLimitError(wrapped), reset, found, guidance, guided)
			require.False(t, found)
			require.False(t, guided)
			require.False(t, isRateLimitError(wrapped))
			require.False(t, strings.Contains(strings.ToLower(pe.Message), "quota"))
		})
	}
}

// Revert-check: Pins cli_active_work.go:22-24's status, context and exact-phrase guards.
func TestHardQuotaBilling1281Guards(t *testing.T) {
	const captured = "Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."
	for _, tc := range []struct {
		name            string
		status          int
		text            string
		contextTooLarge bool
		want            bool
	}{
		{"captured 400", 400, captured, false, true},
		{"captured 402", 402, captured, false, true},
		{"case folded 402", 402, strings.ToUpper(captured), false, true},
		{"max_tokens 400", 400, "max_tokens must be positive", false, false},
		{"unauthorized", 401, captured, false, false},
		{"forbidden", 403, captured, false, false},
		{"overload", 429, "rate_limit_error: server overloaded", false, false},
		{"context", 400, captured, true, false},
		{"unrelated credit", 400, "Your credit balance is too low for another service", false, false},
		{"arbitrary 402", 402, "payment required", false, false},
		{"server error", 500, captured, false, false},
		{"existing quota", 429, "quota exhausted", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pe := &fantasy.ProviderError{StatusCode: tc.status, Message: tc.text, ContextTooLargeErr: tc.contextTooLarge}
			err := &ErrCallAlreadyAttempted{Err: &fantasy.RetryError{Errors: []error{pe}}}
			var reached *fantasy.ProviderError
			require.ErrorAs(t, err, &reached)
			require.Same(t, pe, reached)
			require.Equal(t, tc.want, IsHardQuotaLimit(err))
		})
	}
}
