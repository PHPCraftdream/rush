package agent

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"
)

// Revert-check: observe real adapter fields before asserting classifier policy.
// Fixtures: research rows 37–50, docs/research/2026-10-08-provider-limit-errors.md;
// synthetic envelopes, not reproduced locally against any provider.
func TestProviderLimit1283ObserveAdapters(t *testing.T) {
	for _, name := range []string{"compat", "openai", "azure", "gemini", "vertex", "kimi", "minimax", "bedrock"} {
		t.Run(name, func(t *testing.T) {
			// Revert-check: SDK failure-field retention; research lines 37/38/48, not reproduced locally.
			body := `{"error":{"code":429,"type":"insufficient_quota","message":"quota exceeded","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerDay"}]},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"1800s"}]}}`
			if name == "bedrock" {
				// Revert-check: root Bedrock message retention; research line 50, not reproduced locally.
				body = `{"message":"Too many tokens per day"}`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Logf("request=%s %s", r.Method, r.URL)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "1800")
				w.Header().Set("X-Amzn-Errortype", "ThrottlingException")
				w.WriteHeader(429)
				_, _ = fmt.Fprint(w, body)
			}))
			defer server.Close()
			client := limitTestClient(server)
			var p fantasy.Provider
			var err error
			switch name {
			case "compat":
				p, err = openaicompat.New(openaicompat.WithBaseURL("https://api.deepseek.com/v1"), openaicompat.WithAPIKey("synthetic"), openaicompat.WithHTTPClient(client))
			case "openai":
				p, err = openai.New(openai.WithAPIKey("synthetic"), openai.WithUseResponsesAPI(), openai.WithHTTPClient(client))
			case "azure":
				p, err = azure.New(azure.WithBaseURL("https://synthetic.openai.azure.com"), azure.WithAPIKey("synthetic"), azure.WithUseResponsesAPI(), azure.WithHTTPClient(client))
			case "gemini":
				p, err = google.New(google.WithGeminiAPIKey("synthetic"), google.WithHTTPClient(client))
			case "vertex":
				p, err = google.New(google.WithVertex("synthetic", "us-central1"), google.WithSkipAuth(true), google.WithHTTPClient(client))
			case "kimi":
				p, err = anthropic.New(anthropic.WithBaseURL("https://api.kimi.com/coding"), anthropic.WithAPIKey("synthetic"), anthropic.WithHTTPClient(client))
			case "minimax":
				p, err = anthropic.New(anthropic.WithBaseURL("https://api.minimax.io/anthropic"), anthropic.WithAPIKey("synthetic"), anthropic.WithHTTPClient(client))
			case "bedrock":
				p, err = bedrock.New(bedrock.WithBaseURL("https://bedrock-runtime.us-east-1.amazonaws.com"), bedrock.WithSkipAuth(true), bedrock.WithHTTPClient(client))
			}
			require.NoError(t, err)
			modelID := "gpt-5"
			if name == "gemini" || name == "vertex" {
				modelID = "gemini-synthetic"
			}
			model, err := p.LanguageModel(t.Context(), modelID)
			require.NoError(t, err)
			for _, method := range []string{"generate", "stream"} {
				call := fantasy.Call{Prompt: []fantasy.Message{fantasy.NewUserMessage("synthetic probe")}}
				if method == "generate" {
					_, err = model.Generate(t.Context(), call)
				} else {
					var stream fantasy.StreamResponse
					stream, err = model.Stream(t.Context(), call)
					if err == nil {
						for part := range stream {
							if part.Type == fantasy.StreamPartTypeError {
								err = part.Error
							}
						}
					}
				}
				var pe *fantasy.ProviderError
				require.ErrorAs(t, err, &pe)
				t.Logf("%s URL=%q Title=%q Message=%q Body=%q Headers=%v", method, pe.URL, pe.Title, pe.Message, pe.ResponseBody, pe.ResponseHeaders)
				require.Equal(t, 429, pe.StatusCode)
				require.True(t, IsHardQuotaLimit(err))
				if name == "openai" || name == "azure" {
					require.Contains(t, pe.URL, "/responses")
				}
				if name == "gemini" || name == "vertex" {
					var cause genai.APIError
					require.True(t, errors.As(pe.Cause, &cause))
					require.Len(t, cause.Details, 2)
					require.Empty(t, pe.URL)
					require.Empty(t, pe.ResponseHeaders)
					t.Logf("Cause.Details=%v", cause.Details)
				} else {
					require.NotEmpty(t, pe.URL)
					require.Contains(t, string(pe.ResponseBody), "HTTP/")
					require.Equal(t, "1800", pe.ResponseHeaders["Retry-After"])
				}
			}
		})
	}
}

// Revert-check: compat billing and DashScope exceptions must reach public policy.
// Sources: https://api-docs.deepseek.com/quick_start/error_codes/ (row 42),
// https://www.alibabacloud.com/help/en/model-studio/error-code (row 43);
// not reproduced locally against either provider.
func TestProviderLimit1283CompatRegression(t *testing.T) {
	for _, tc := range []struct {
		name, base, body string
		status           int
		hard             bool
	}{
		// Revert-check: DeepSeek billing wall; research line 42, not reproduced locally.
		{"deepseek402", "https://api.deepseek.com/v1", `{"error":{"message":"Insufficient Balance"}}`, 402, true},
		// Revert-check: DashScope quota exception; research line 43, not reproduced locally.
		{"dashscope", "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", `{"error":{"code":"insufficient_quota","message":"Allocated quota exceeded, please increase your quota limit"}}`, 429, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			p, err := openaicompat.New(openaicompat.WithBaseURL(tc.base), openaicompat.WithAPIKey("synthetic"), openaicompat.WithHTTPClient(limitTestClient(server)))
			require.NoError(t, err)
			model, err := p.LanguageModel(t.Context(), "synthetic")
			require.NoError(t, err)
			_, err = model.Generate(t.Context(), fantasy.Call{})
			var pe *fantasy.ProviderError
			require.ErrorAs(t, err, &pe)
			t.Logf("URL=%q Title=%q Message=%q Body=%q Headers=%v", pe.URL, pe.Title, pe.Message, pe.ResponseBody, pe.ResponseHeaders)
			require.Equal(t, tc.hard, IsHardQuotaLimit(err))
		})
	}
}
