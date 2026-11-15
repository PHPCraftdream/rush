package limitwords

import (
	"fmt"
	"testing"
	"time"
)

// Revert-check: provider exceptions precede generic quota/rate wording.
// All synthetic fixtures cite research rows 37–50 in
// docs/research/2026-10-08-provider-limit-errors.md; not reproduced locally.
func TestProviderRules1283(t *testing.T) {
	tests := []struct {
		provider, code, message string
		status                  int
		want                    Class
	}{
		// Revert-check: Moonshot business types; research line 40, not reproduced locally.
		{"moonshot", "exceeded_current_quota_error", "balance", 429, Hard},
		// Revert-check: Moonshot business types; research line 40, not reproduced locally.
		{"moonshot", "rate_limit_reached_error", "quota", 429, Transient},
		// Revert-check: Moonshot business types; research line 40, not reproduced locally.
		{"moonshot", "engine_overloaded_error", "quota", 429, Transient},
		// Revert-check: MiniMax business walls/rate; research line 41, not reproduced locally.
		{"minimax", "1008", "balance", 429, Hard},
		// Revert-check: MiniMax business walls/rate; research line 41, not reproduced locally.
		{"minimax", "2056", "usage", 429, Hard},
		// Revert-check: MiniMax business walls/rate; research line 41, not reproduced locally.
		{"minimax", "1002", "quota", 429, Transient},
		// Revert-check: MiniMax business walls/rate; research line 41, not reproduced locally.
		{"minimax", "", "api_error: insufficient balance (1008)", 500, Hard},
		// Revert-check: MiniMax business walls/rate; research line 41, not reproduced locally.
		{"minimax", "2056", "usage", 500, Hard},
		// Revert-check: MiniMax business walls/rate; research line 41, not reproduced locally.
		{"minimax", "", "server overloaded", 500, Unknown},
		// Revert-check: DeepSeek status policy; research line 42, not reproduced locally.
		{"deepseek", "", "Insufficient Balance", 402, Hard},
		// Revert-check: DeepSeek status policy; research line 42, not reproduced locally.
		{"deepseek", "", "quota", 429, Transient},
		// Revert-check: DeepSeek status policy; research line 42, not reproduced locally.
		{"deepseek", "", "Server Overloaded", 503, Transient},
		// Revert-check: DashScope billing versus TPS; research line 43, not reproduced locally.
		{"dashscope", "Throttling.AllocationQuota", "quota", 429, Transient},
		// Revert-check: DashScope billing versus TPS; research line 43, not reproduced locally.
		{"dashscope", "insufficient_quota", "quota", 429, Transient},
		// Revert-check: DashScope billing versus TPS; research line 43, not reproduced locally.
		{"dashscope", "Throttling.RateQuota", "quota", 429, Transient},
		// Revert-check: DashScope billing versus TPS; research line 43, not reproduced locally.
		{"dashscope", "LimitRequests", "quota", 429, Transient},
		// Revert-check: DashScope billing versus TPS; research line 43, not reproduced locally.
		{"dashscope", "", "Allocated quota exceeded, please increase your quota limit", 429, Transient},
		// Revert-check: DashScope billing versus TPS; research line 43, not reproduced locally.
		{"dashscope", "PrepaidBillOverdue", "billing", 429, Hard},
		// Revert-check: DashScope billing versus TPS; research line 43, not reproduced locally.
		{"dashscope", "PostpaidBillOverdue", "billing", 429, Hard},
		// Revert-check: DashScope billing versus TPS; research line 43, not reproduced locally.
		{"dashscope", "Arrearage", "billing", 429, Hard},
		// Revert-check: DashScope billing versus TPS; research line 43, not reproduced locally.
		{"dashscope", "", "Free allocated quota exceeded", 429, Hard},
		// Revert-check: DashScope billing versus TPS; research line 43, not reproduced locally.
		{"dashscope", "", "hour allocated quota exhausted", 429, Hard},
		// Revert-check: DashScope billing versus TPS; research line 43, not reproduced locally.
		{"dashscope", "", "week allocated quota exhausted", 429, Hard},
		// Revert-check: DashScope billing versus TPS; research line 43, not reproduced locally.
		{"dashscope", "", "month allocated quota exhausted", 429, Hard},
		// Revert-check: OpenAI quota/rate distinction; research line 37, not reproduced locally.
		{"openai", "insufficient_quota", "billing", 429, Hard},
		// Revert-check: fallback/negative isolation; research lines 19–27/37, not reproduced locally.
		{"", "insufficient_quota", "billing", 429, Hard},
		// Revert-check: fallback/negative isolation; research lines 19–27/37, not reproduced locally.
		{"", "", "You exceeded your current quota, please check your plan and billing details", 429, Hard},
		// Revert-check: OpenAI quota/rate distinction; research line 37, not reproduced locally.
		{"openai", "rate_limit_exceeded", "quota", 429, Transient},
		// Revert-check: Azure quota exception; research line 38, not reproduced locally.
		{"azure", "insufficient_quota", "quota", 429, Transient},
		// Revert-check: Azure quota exception; research line 38, not reproduced locally.
		{"azure", "", "token rate limit of current pricing tier quota", 429, Transient},
		// Revert-check: OpenRouter billing/free throttle; research line 45, not reproduced locally.
		{"openrouter", "", "Insufficient credits", 402, Hard},
		// Revert-check: OpenRouter billing/free throttle; research line 45, not reproduced locally.
		{"openrouter", "", "free model quota", 429, Transient},
		// Revert-check: xAI spend versus rate; research line 46, not reproduced locally.
		{"xai", "", "used all available credits", 429, Hard},
		// Revert-check: xAI spend versus rate; research line 46, not reproduced locally.
		{"xai", "", "reached monthly spending limit", 429, Hard},
		// Revert-check: xAI spend versus rate; research line 46, not reproduced locally.
		{"xai", "", "per-model RPS", 429, Transient},
		// Revert-check: xAI spend versus rate; research line 46, not reproduced locally.
		{"xai", "", "quota TPM", 429, Transient},
		// Revert-check: Groq daily versus minute; research line 47, not reproduced locally.
		{"groq", "rate_limit_exceeded", "tokens per day TPD requests", 429, Hard},
		// Revert-check: Groq daily versus minute; research line 47, not reproduced locally.
		{"groq", "rate_limit_exceeded", "quota TPM", 429, Transient},
		// Revert-check: Groq daily versus minute; research line 47, not reproduced locally.
		{"groq", "", "quota RPM", 429, Transient},
		// Revert-check: Mistral rate shape; research line 49, not reproduced locally.
		{"mistral", "rate_limited", "Rate limit exceeded", 429, Transient},
		// Revert-check: Mistral rate shape; research line 49, not reproduced locally.
		{"mistral", "1300", "Requests rate limit exceeded", 429, Transient},
		// Revert-check: Bedrock daily versus throttle; research line 50, not reproduced locally.
		{"bedrock", "ThrottlingException", "Too many requests", 429, Transient},
		// Revert-check: Bedrock daily versus throttle; research line 50, not reproduced locally.
		{"bedrock", "ThrottlingException", "Too many tokens per day", 429, Hard},
		// Revert-check: fallback/negative isolation; research lines 19–27/37, not reproduced locally.
		{"", "1008", "balance", 429, Unknown},
		// Revert-check: fallback/negative isolation; research lines 19–27/37, not reproduced locally.
		{"", "2056", "balance", 500, Unknown},
		// Revert-check: OpenAI quota/rate distinction; research line 37, not reproduced locally.
		{"openai", "", "max_tokens must be positive", 400, Unknown},
		// Revert-check: MiniMax business walls/rate; research line 41, not reproduced locally.
		{"minimax", "1008", "balance", 401, Unknown},
		// Revert-check: DashScope billing versus TPS; research line 43, not reproduced locally.
		{"dashscope", "Arrearage", "billing", 403, Unknown},
		// Revert-check: fallback/negative isolation; research lines 19–27/37, not reproduced locally.
		{"", "", "assistant says insufficient balance", 0, Unknown},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%s/%s/%d", tc.provider, tc.code, i), func(t *testing.T) {
			body := fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, tc.code, tc.message)
			got := Classify(Input{Provider: tc.provider, Status: tc.status, Body: body})
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
	// Revert-check: numeric MiniMax business envelopes; research line 41, not reproduced locally.
	for _, code := range []int{1008, 2056, 1002} {
		want := Hard
		if code == 1002 {
			want = Transient
		}
		got := Classify(Input{Provider: "minimax", Status: 429, Body: fmt.Sprintf(`{"base_resp":{"status_code":%d,"status_msg":"balance"}}`, code)})
		if got != want {
			t.Fatalf("numeric business code %d: got %v want %v", code, got, want)
		}
	}
}

// Revert-check: duration boundary and typed daily facts cannot be flattened away.
// Research rows 38,47,48; synthetic, not reproduced locally.
func TestProviderDurations1283(t *testing.T) {
	// Revert-check: Azure duration boundary; research lines 23–25/38, not reproduced locally.
	for _, seconds := range []int{1799, 1800, 86400} {
		want := Transient
		if seconds >= 1800 {
			want = Hard
		}
		if got := Classify(Input{Provider: "azure", Status: 429, Message: "quota", ResetAfter: time.Duration(seconds) * time.Second}); got != want {
			t.Fatalf("azure %d: %v", seconds, got)
		}
	}
	for _, tc := range []struct {
		text  string
		delay time.Duration
		want  Class
	}{
		// Revert-check: Groq short wait; research line 47, not reproduced locally.
		{"Please try again in 9m38s", 9*time.Minute + 38*time.Second, Transient},
		// Revert-check: Groq long wait; research lines 23–25/47, not reproduced locally.
		{"Please try again in 1h2m3s", time.Hour + 2*time.Minute + 3*time.Second, Hard},
	} {
		if got := GroqRetryDelay(tc.text); got != tc.delay {
			t.Fatalf("duration %q: %v", tc.text, got)
		}
		if got := Classify(Input{Provider: "groq", Status: 429, Message: "quota TPM. " + tc.text}); got != tc.want {
			t.Fatalf("groq %q: %v", tc.text, got)
		}
	}
	// Revert-check: Gemini daily/minute RetryInfo; research line 48, not reproduced locally.
	for _, id := range []string{"GenerateRequestsPerDay", "GenerateRequestsPerMinute"} {
		for _, seconds := range []int{1, 1799, 1800} {
			body := fmt.Sprintf(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"quota requests","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":%q}]},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"%ds"}]}}`, id, seconds)
			want := Transient
			if id == "GenerateRequestsPerDay" || seconds >= 1800 {
				want = Hard
			}
			if got := Classify(Input{Provider: "gemini", Status: 429, Body: body}); got != want {
				t.Fatalf("gemini %s/%d got %v want %v", id, seconds, got, want)
			}
		}
	}
}
