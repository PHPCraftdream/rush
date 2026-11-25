package agent

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/clibridge"
	usagepolicy "github.com/PHPCraftdream/rush/internal/agent/runtime/policy/usage"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"
)

// Revert-check: exact hosts beat hints, suffix impostors and unrelated codes.
// Research rows 37–50; synthetic fixtures, not reproduced locally.
func TestProviderLimit1283Identity(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		host, code, message string
		status              int
		want                usagepolicy.ProviderLimitClass
	}{
		// Revert-check: Moonshot host identity; research line 40, not reproduced locally.
		{"api.moonshot.ai", "exceeded_current_quota_error", "balance", 429, usagepolicy.ProviderLimitHard},
		// Revert-check: Moonshot host identity; research line 40, not reproduced locally.
		{"api.moonshot.cn", "rate_limit_reached_error", "quota", 429, usagepolicy.ProviderLimitTransient},
		// Revert-check: Kimi serving identity; research line 40, not reproduced locally.
		{"api.kimi.com", "engine_overloaded_error", "quota", 429, usagepolicy.ProviderLimitTransient},
		// Revert-check: Kimi platform identity; research line 40, not reproduced locally.
		{"platform.kimi.ai", "exceeded_current_quota_error", "balance", 429, usagepolicy.ProviderLimitHard},
		// Revert-check: MiniMax host/auth isolation; research lines 19–27/41, not reproduced locally.
		{"api.minimax.io", "1008", "balance", 429, usagepolicy.ProviderLimitHard},
		// Revert-check: MiniMax host/auth isolation; research lines 19–27/41, not reproduced locally.
		{"api.minimaxi.com", "2056", "usage", 429, usagepolicy.ProviderLimitHard},
		// Revert-check: DeepSeek exact host; research lines 19–27/42, not reproduced locally.
		{"api.deepseek.com", "", "balance", 402, usagepolicy.ProviderLimitHard},
		// Revert-check: DashScope host exception; research line 43, not reproduced locally.
		{"dashscope.aliyuncs.com", "insufficient_quota", "quota", 429, usagepolicy.ProviderLimitTransient},
		// Revert-check: DashScope host exception; research line 43, not reproduced locally.
		{"dashscope-intl.aliyuncs.com", "Throttling.AllocationQuota", "quota", 429, usagepolicy.ProviderLimitTransient},
		// Revert-check: coding endpoint exception; research line 43, not reproduced locally.
		{"coding.dashscope.aliyuncs.com", "Arrearage", "billing", 429, usagepolicy.ProviderLimitHard},
		// Revert-check: coding endpoint exception; research line 43, not reproduced locally.
		{"coding-intl.dashscope.aliyuncs.com", "insufficient_quota", "quota", 429, usagepolicy.ProviderLimitTransient},
		// Revert-check: OpenAI serving host; research line 37, not reproduced locally.
		{"api.openai.com", "insufficient_quota", "billing", 429, usagepolicy.ProviderLimitHard},
		// Revert-check: Azure safe suffix; research lines 19–27/38, not reproduced locally.
		{"tenant.openai.azure.com", "insufficient_quota", "quota", 429, usagepolicy.ProviderLimitTransient},
		// Revert-check: Azure safe suffix; research lines 19–27/38, not reproduced locally.
		{"tenant.cognitiveservices.azure.com", "insufficient_quota", "quota", 429, usagepolicy.ProviderLimitTransient},
		// Revert-check: OpenRouter serving host; research line 45, not reproduced locally.
		{"openrouter.ai", "", "credits", 402, usagepolicy.ProviderLimitHard},
		// Revert-check: xAI serving host; research line 46, not reproduced locally.
		{"api.x.ai", "", "monthly spending limit", 429, usagepolicy.ProviderLimitHard},
		// Revert-check: Groq serving host; research line 47, not reproduced locally.
		{"api.groq.com", "rate_limit_exceeded", "tokens per day", 429, usagepolicy.ProviderLimitHard},
		// Revert-check: Mistral serving host; research line 49, not reproduced locally.
		{"api.mistral.ai", "rate_limited", "rate limit exceeded", 429, usagepolicy.ProviderLimitTransient},
		// Revert-check: Bedrock safe suffix; research lines 19–27/50, not reproduced locally.
		{"bedrock-runtime.us-east-1.amazonaws.com", "ThrottlingException", "Too many tokens per day", 429, usagepolicy.ProviderLimitHard},
		// Revert-check: DeepSeek exact host; research lines 19–27/42, not reproduced locally.
		{"api.deepseek.com.evil.invalid", "", "balance", 402, usagepolicy.ProviderLimitUnknown},
		// Revert-check: MiniMax host/auth isolation; research lines 19–27/41, not reproduced locally.
		{"api.minimax.io.evil.invalid", "1008", "balance", 429, usagepolicy.ProviderLimitUnknown},
		// Revert-check: Azure safe suffix; research lines 19–27/38, not reproduced locally.
		{"tenant.openai.azure.com.evil.invalid", "", "balance", 402, usagepolicy.ProviderLimitUnknown},
		// Revert-check: Bedrock safe suffix; research lines 19–27/50, not reproduced locally.
		{"bedrock-runtime.us-east-1.amazonaws.com.evil.invalid", "ThrottlingException", "Too many tokens per day", 429, usagepolicy.ProviderLimitUnknown},
		// Revert-check: exact host match; a host that merely ends with a known serving host is not that provider.
		{"evil-api.deepseek.com", "", "balance", 402, usagepolicy.ProviderLimitUnknown},
		// Revert-check: MiniMax host/auth isolation; research lines 19–27/41, not reproduced locally.
		{"api.minimax.io", "1008", "balance", 401, usagepolicy.ProviderLimitUnknown},
		// Revert-check: MiniMax host/auth isolation; research lines 19–27/41, not reproduced locally.
		{"api.minimax.io", "1008", "balance", 403, usagepolicy.ProviderLimitUnknown},
	} {
		t.Run(tc.host+tc.code+fmt.Sprint(tc.status), func(t *testing.T) {
			pe := &fantasy.ProviderError{URL: "https://" + tc.host + "/v1", StatusCode: tc.status, ResponseBody: []byte(fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, tc.code, tc.message))}
			require.Equal(t, tc.want, usagepolicy.ClassifyHardProviderLimit(pe, "", now, usagepolicy.DisplayZoneProvider))
			if tc.want == usagepolicy.ProviderLimitHard {
				pe.ContextTooLargeErr = true
				require.Equal(t, usagepolicy.ProviderLimitUnknown, usagepolicy.ClassifyHardProviderLimit(pe, "", now, usagepolicy.DisplayZoneProvider))
			}
		})
	}
	// Revert-check: host beats alias; research line 43, not reproduced locally.
	pe := &fantasy.ProviderError{URL: "https://dashscope.aliyuncs.com/v1", StatusCode: 429, ResponseBody: []byte(`{"error":{"code":"insufficient_quota","message":"quota"}}`)}
	require.Equal(t, usagepolicy.ProviderLimitTransient, usagepolicy.ClassifyHardProviderLimit(pe, "openai", now, usagepolicy.DisplayZoneProvider))
	// Revert-check: Azure duration boundary; research lines 23–25/38, not reproduced locally.
	for _, seconds := range []int{1799, 1800, 86400} {
		pe := &fantasy.ProviderError{URL: "https://tenant.openai.azure.com", StatusCode: 429, Message: "quota", ResponseHeaders: map[string]string{"Retry-After": fmt.Sprint(seconds)}}
		want := usagepolicy.ProviderLimitTransient
		if seconds >= 1800 {
			want = usagepolicy.ProviderLimitHard
		}
		require.Equal(t, want, usagepolicy.ClassifyHardProviderLimit(pe, "", now, usagepolicy.DisplayZoneProvider))
	}
}

// Revert-check: failure channels only; research lines 19–27, not reproduced locally.
func TestProviderLimit1283NonFailure(t *testing.T) {
	require.False(t, clibridge.IsHardQuotaLimit(errors.New("assistant says quota exhausted and insufficient_quota")))
	pe := &fantasy.ProviderError{
		URL: "https://api.openai.com/v1/responses", StatusCode: 200,
		ResponseBody: []byte(`{"output":[{"content":[{"text":"quota exhausted insufficient_quota"}]}]}`),
	}
	require.False(t, clibridge.IsHardQuotaLimit(pe))
}

// Revert-check: recover Google details without altering Fantasy's captured fields.
// Research row 48, google-gemini/gemini-cli #8883; not reproduced locally.
func TestProviderLimit1283GoogleCause(t *testing.T) {
	for _, id := range []string{"GenerateRequestsPerDay", "GenerateRequestsPerMinute"} {
		for _, delay := range []string{"1s", "1799s", "1800s"} {
			// Revert-check: quotaId and RetryInfo survive Cause; research line 48, not reproduced locally.
			cause := genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "quota requests", Details: []map[string]any{
				{"@type": "type.googleapis.com/google.rpc.QuotaFailure", "violations": []any{map[string]any{"quotaId": id}}},
				{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": delay},
			}}
			pe := &fantasy.ProviderError{StatusCode: 429, Message: cause.Message, ResponseBody: []byte(cause.Message), Cause: fmt.Errorf("wrapped: %w", cause)}
			want := usagepolicy.ProviderLimitTransient
			if id == "GenerateRequestsPerDay" || delay == "1800s" {
				want = usagepolicy.ProviderLimitHard
			}
			require.Equal(t, want, usagepolicy.ClassifyHardProviderLimit(pe, "", time.Now(), usagepolicy.DisplayZoneProvider))
			require.Equal(t, cause.Message, string(pe.ResponseBody))
			require.Empty(t, pe.URL)
		}
	}
}
