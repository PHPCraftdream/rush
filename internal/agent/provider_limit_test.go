package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// Revert-check: limitwords.Classify's status, keyword and code branches pin every row.
func TestProviderLimit1282Classification(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name, message, url, body, hint string
		status                         int
		context                        bool
		want                           providerLimitClass
	}{
		{name: "usage", message: "usage limit", status: 429, want: providerLimitHard},
		{name: "will reset", message: "limit will reset", status: 429, want: providerLimitHard},
		{name: "reset", message: "reset at", status: 429, want: providerLimitHard},
		{name: "quota", message: "quota exhausted", status: 429, want: providerLimitHard},
		// https://github.com/anthropics/claude-code/issues/19673; not reproduced locally.
		{name: "curly", message: "You’ve hit your limit", status: 429, want: providerLimitHard},
		{name: "straight", message: "You've hit your limit", status: 429, want: providerLimitHard},
		{name: "minute", message: "quota per minute", status: 429, want: providerLimitTransient},
		{name: "tpm", message: "quota TPM", status: 429, want: providerLimitTransient},
		{name: "rpm", message: "quota RPM", status: 429, want: providerLimitTransient},
		{name: "requests", message: "quota requests", status: 429, want: providerLimitTransient},
		{name: "overload", message: "quota overloaded", status: 429, want: providerLimitTransient},
		{name: "later", message: "quota try again later", status: 429, want: providerLimitTransient},
		{name: "rate code", message: "quota rate_limit_exceeded", status: 429, want: providerLimitTransient},
		{name: "max tokens", message: "max_tokens must be positive", status: 400, want: providerLimitUnknown},
		{name: "ordinary400", message: "quota", status: 400, want: providerLimitUnknown},
		{name: "auth", message: "quota", status: 401, want: providerLimitUnknown},
		{name: "forbidden", message: "quota", status: 403, want: providerLimitUnknown},
		{name: "server", message: "usage limit", status: 500, want: providerLimitUnknown},
		{name: "context", message: "quota", status: 429, context: true, want: providerLimitUnknown},
		{name: "unknown", message: "slow down", status: 429, want: providerLimitUnknown},
		{name: "unscoped402", message: "insufficient balance", status: 402, want: providerLimitUnknown},
		{name: "step402", url: "https://api.stepfun.ai/v1/chat/completions", message: "insufficient balance", status: 402, want: providerLimitHard},
		{name: "step hint", hint: "stepfun", message: "insufficient balance", status: 402, want: providerLimitHard},
		{name: "step marker", message: "StepFun insufficient balance", status: 402, want: providerLimitHard},
		{name: "step429", url: "https://api.stepfun.ai/v1/chat/completions", message: "quota usage limit", status: 429, want: providerLimitTransient},
		{name: "step impostor", url: "https://api.stepfun.ai.evil.invalid/v1", message: "insufficient balance", status: 402, want: providerLimitUnknown},
		{name: "step query", url: "https://example.invalid/?provider=api.stepfun.ai", message: "insufficient balance", status: 402, want: providerLimitUnknown},
		// https://github.com/acmiyaguchi/fen/issues/583; not reproduced locally.
		{name: "codex type", body: `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`, status: 429, want: providerLimitHard},
		{name: "codex SSE", body: `{"response":{"error":{"code":"usage_limit_reached","message":"wall"}}}`, status: 0, want: providerLimitHard},
		{name: "codex uppercase type", body: `{"error":{"type":"USAGE_LIMIT_REACHED","message":"wall"}}`, status: 0, want: providerLimitHard},
		{name: "codex lowercase type wall", body: `{"error":{"type":"usage_limit_reached","message":"wall"}}`, status: 0, want: providerLimitHard},
		{name: "codex uppercase code", body: `{"response":{"error":{"code":"USAGE_LIMIT_REACHED","message":"wall"}}}`, status: 0, want: providerLimitHard},
		{name: "rate uppercase type", body: `{"error":{"type":"RATE_LIMIT_EXCEEDED","message":"quota"}}`, status: 429, want: providerLimitTransient},
		{name: "rate lowercase type", body: `{"error":{"type":"rate_limit_exceeded","message":"quota"}}`, status: 429, want: providerLimitTransient},
		{name: "rate uppercase code", body: `{"error":{"code":"RATE_LIMIT_EXCEEDED","message":"quota"}}`, status: 429, want: providerLimitTransient},
		{name: "rate lowercase code", body: `{"error":{"code":"rate_limit_exceeded","message":"quota"}}`, status: 429, want: providerLimitTransient},
		{name: "codex auth", body: `{"error":{"type":"usage_limit_reached"}}`, status: 401, want: providerLimitUnknown},
	}
	// Vendor fixtures: research spec sources 57/58/63; not reproduced locally.
	// https://docs.z.ai/api-reference/api-code; https://platform.stepfun.ai/docs/en/api-reference/error-codes
	for _, code := range []string{"1113", "1308", "1309", "1310", "1316", "1317", "1318", "1319", "1320", "1321", "1302", "1305"} {
		want := providerLimitHard
		if code == "1302" || code == "1305" {
			want = providerLimitTransient
		}
		for _, numeric := range []bool{false, true} {
			value := any(code)
			if numeric {
				value = json.Number(code)
			}
			body, err := json.Marshal(map[string]any{"error": map[string]any{"code": value, "message": "quota"}})
			require.NoError(t, err)
			tests = append(tests, struct {
				name, message, url, body, hint string
				status                         int
				context                        bool
				want                           providerLimitClass
			}{name: fmt.Sprintf("zai %s numeric=%v", code, numeric), body: string(body), status: 429, want: want})
		}
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pe := &fantasy.ProviderError{Message: tc.message, URL: tc.url, ResponseBody: []byte(tc.body), StatusCode: tc.status, ContextTooLargeErr: tc.context}
			require.Equal(t, tc.want, classifyHardProviderLimit(pe, tc.hint, now))
			if tc.hint == "" {
				require.Equal(t, tc.want == providerLimitHard, IsHardQuotaLimit(fmt.Errorf("wrapped: %w", pe)))
				require.Equal(t, tc.want == providerLimitHard, isQuotaLimit(pe))
			}
		})
	}
	require.False(t, IsHardQuotaLimit(errors.New("quota exhausted HTTP 429")))
	require.False(t, IsHardQuotaLimit(nil))
	require.False(t, isQuotaLimit(nil))
}

// Revert-check: hasLongLimitReset must evaluate every uncapped hint at the 30m boundary.
func TestProviderLimit1282ResetBoundary(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for _, seconds := range []int{1799, 1800, 1801} {
		for _, name := range []string{"Retry-After", "retry-after-ms", "x-ratelimit-reset-tokens", "anthropic-ratelimit-tokens-reset", "resets_in_seconds", "resets_at", "zai"} {
			t.Run(fmt.Sprintf("%s/%d", name, seconds), func(t *testing.T) {
				pe := &fantasy.ProviderError{StatusCode: 429, Message: "quota requests", ResponseHeaders: map[string]string{}}
				switch name {
				case "Retry-After":
					pe.ResponseHeaders[name] = fmt.Sprint(seconds)
				case "retry-after-ms":
					pe.ResponseHeaders[name] = fmt.Sprint(seconds * 1000)
				case "x-ratelimit-reset-tokens":
					pe.ResponseHeaders[name] = fmt.Sprintf("%ds", seconds)
				case "anthropic-ratelimit-tokens-reset":
					pe.ResponseHeaders[name] = now.Add(time.Duration(seconds) * time.Second).Format(time.RFC3339)
				case "resets_in_seconds":
					pe.ResponseBody = []byte(fmt.Sprintf(`{"error":{"resets_in_seconds":%d}}`, seconds))
				case "resets_at":
					pe.ResponseBody = []byte(fmt.Sprintf(`{"error":{"resets_at":%d}}`, now.Unix()+int64(seconds)))
				case "zai":
					pe.Message += ". Your limit will reset at " + now.Add(time.Duration(seconds)*time.Second).Format("2006-01-02 15:04:05") + "Z"
				}
				want := providerLimitTransient
				if seconds >= 1800 {
					want = providerLimitHard
				}
				require.Equal(t, want, classifyHardProviderLimit(pe, "", now))
			})
		}
	}
	pe := &fantasy.ProviderError{StatusCode: 429, URL: "https://api.stepfun.ai/v1", Message: "quota", ResponseHeaders: map[string]string{"Retry-After": "1", "X-Ratelimit-Reset-Tokens": "30m"}, ResponseBody: []byte(`{"error":{"resets_in_seconds":1,"resets_at":1791462600}}`)}
	require.Equal(t, providerLimitHard, classifyHardProviderLimit(pe, "", now))
	reset, ok := QuotaLimitResetTime(pe, now)
	require.True(t, ok)
	require.Equal(t, now.Add(time.Second), reset)
	pe.ResponseHeaders = nil
	pe.ResponseBody = []byte(fmt.Sprintf(`{"error":{"resets_in_seconds":1,"resets_at":%d}}`, now.Add(time.Hour).Unix()))
	require.Equal(t, providerLimitHard, classifyHardProviderLimit(pe, "", now))
	require.Equal(t, providerLimitUnknown, classifyHardProviderLimit(&fantasy.ProviderError{StatusCode: 503, ResponseHeaders: map[string]string{"Retry-After": "1800"}}, "", now))
	_, ok = QuotaLimitResetTime(nil, now)
	require.False(t, ok)
}
