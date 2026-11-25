package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/clibridge"
	usagepolicy "github.com/PHPCraftdream/rush/internal/agent/runtime/policy/usage"

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
		want                           usagepolicy.ProviderLimitClass
	}{
		{name: "usage", message: "usage limit", status: 429, want: usagepolicy.ProviderLimitHard},
		{name: "will reset", message: "limit will reset", status: 429, want: usagepolicy.ProviderLimitHard},
		{name: "reset", message: "reset at", status: 429, want: usagepolicy.ProviderLimitHard},
		{name: "quota", message: "quota exhausted", status: 429, want: usagepolicy.ProviderLimitHard},
		// https://github.com/anthropics/claude-code/issues/19673; not reproduced locally.
		{name: "curly", message: "You’ve hit your limit", status: 429, want: usagepolicy.ProviderLimitHard},
		{name: "straight", message: "You've hit your limit", status: 429, want: usagepolicy.ProviderLimitHard},
		{name: "minute", message: "quota per minute", status: 429, want: usagepolicy.ProviderLimitTransient},
		{name: "tpm", message: "quota TPM", status: 429, want: usagepolicy.ProviderLimitTransient},
		{name: "rpm", message: "quota RPM", status: 429, want: usagepolicy.ProviderLimitTransient},
		{name: "requests", message: "quota requests", status: 429, want: usagepolicy.ProviderLimitTransient},
		{name: "overload", message: "quota overloaded", status: 429, want: usagepolicy.ProviderLimitTransient},
		{name: "later", message: "quota try again later", status: 429, want: usagepolicy.ProviderLimitTransient},
		{name: "rate code", message: "quota rate_limit_exceeded", status: 429, want: usagepolicy.ProviderLimitTransient},
		{name: "max tokens", message: "max_tokens must be positive", status: 400, want: usagepolicy.ProviderLimitUnknown},
		{name: "ordinary400", message: "quota", status: 400, want: usagepolicy.ProviderLimitUnknown},
		{name: "auth", message: "quota", status: 401, want: usagepolicy.ProviderLimitUnknown},
		{name: "forbidden", message: "quota", status: 403, want: usagepolicy.ProviderLimitUnknown},
		{name: "server", message: "usage limit", status: 500, want: usagepolicy.ProviderLimitUnknown},
		{name: "context", message: "quota", status: 429, context: true, want: usagepolicy.ProviderLimitUnknown},
		{name: "unknown", message: "slow down", status: 429, want: usagepolicy.ProviderLimitUnknown},
		{name: "unscoped402", message: "insufficient balance", status: 402, want: usagepolicy.ProviderLimitUnknown},
		{name: "step402", url: "https://api.stepfun.ai/v1/chat/completions", message: "insufficient balance", status: 402, want: usagepolicy.ProviderLimitHard},
		{name: "step hint", hint: "stepfun", message: "insufficient balance", status: 402, want: usagepolicy.ProviderLimitHard},
		{name: "step marker", message: "StepFun insufficient balance", status: 402, want: usagepolicy.ProviderLimitHard},
		{name: "step429", url: "https://api.stepfun.ai/v1/chat/completions", message: "quota usage limit", status: 429, want: usagepolicy.ProviderLimitTransient},
		{name: "step impostor", url: "https://api.stepfun.ai.evil.invalid/v1", message: "insufficient balance", status: 402, want: usagepolicy.ProviderLimitUnknown},
		{name: "step query", url: "https://example.invalid/?provider=api.stepfun.ai", message: "insufficient balance", status: 402, want: usagepolicy.ProviderLimitUnknown},
		// https://github.com/acmiyaguchi/fen/issues/583; not reproduced locally.
		{name: "codex type", body: `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`, status: 429, want: usagepolicy.ProviderLimitHard},
		{name: "codex SSE", body: `{"response":{"error":{"code":"usage_limit_reached","message":"wall"}}}`, status: 0, want: usagepolicy.ProviderLimitHard},
		{name: "codex uppercase type", body: `{"error":{"type":"USAGE_LIMIT_REACHED","message":"wall"}}`, status: 0, want: usagepolicy.ProviderLimitHard},
		{name: "codex lowercase type wall", body: `{"error":{"type":"usage_limit_reached","message":"wall"}}`, status: 0, want: usagepolicy.ProviderLimitHard},
		{name: "codex uppercase code", body: `{"response":{"error":{"code":"USAGE_LIMIT_REACHED","message":"wall"}}}`, status: 0, want: usagepolicy.ProviderLimitHard},
		{name: "rate uppercase type", body: `{"error":{"type":"RATE_LIMIT_EXCEEDED","message":"quota"}}`, status: 429, want: usagepolicy.ProviderLimitTransient},
		{name: "rate lowercase type", body: `{"error":{"type":"rate_limit_exceeded","message":"quota"}}`, status: 429, want: usagepolicy.ProviderLimitTransient},
		{name: "rate uppercase code", body: `{"error":{"code":"RATE_LIMIT_EXCEEDED","message":"quota"}}`, status: 429, want: usagepolicy.ProviderLimitTransient},
		{name: "rate lowercase code", body: `{"error":{"code":"rate_limit_exceeded","message":"quota"}}`, status: 429, want: usagepolicy.ProviderLimitTransient},
		{name: "codex auth", body: `{"error":{"type":"usage_limit_reached"}}`, status: 401, want: usagepolicy.ProviderLimitUnknown},
	}
	// Vendor fixtures: research spec sources 57/58/63; not reproduced locally.
	// https://docs.z.ai/api-reference/api-code; https://platform.stepfun.ai/docs/en/api-reference/error-codes
	for _, tc := range []struct {
		code string
		want usagepolicy.ProviderLimitClass
	}{
		{"1113", usagepolicy.ProviderLimitHard},
		{"1308", usagepolicy.ProviderLimitHard},
		{"1309", usagepolicy.ProviderLimitHard},
		{"1310", usagepolicy.ProviderLimitHard},
		{"1316", usagepolicy.ProviderLimitHard},
		{"1317", usagepolicy.ProviderLimitHard},
		{"1318", usagepolicy.ProviderLimitHard},
		{"1319", usagepolicy.ProviderLimitHard},
		{"1320", usagepolicy.ProviderLimitHard},
		{"1321", usagepolicy.ProviderLimitHard},
		{"1302", usagepolicy.ProviderLimitTransient},
		{"1305", usagepolicy.ProviderLimitTransient},
	} {
		code := tc.code
		for _, numeric := range []bool{false, true} {
			value := any(code)
			if numeric {
				value = json.Number(code)
			}
			body, err := json.Marshal(map[string]any{"error": map[string]any{"code": value, "message": "neutral fixture"}})
			require.NoError(t, err)
			tests = append(tests, struct {
				name, message, url, body, hint string
				status                         int
				context                        bool
				want                           usagepolicy.ProviderLimitClass
			}{name: fmt.Sprintf("zai %s numeric=%v", code, numeric), body: string(body), status: 429, want: tc.want})
		}
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pe := &fantasy.ProviderError{Message: tc.message, URL: tc.url, ResponseBody: []byte(tc.body), StatusCode: tc.status, ContextTooLargeErr: tc.context}
			require.Equal(t, tc.want, usagepolicy.ClassifyHardProviderLimit(pe, tc.hint, now, usagepolicy.DisplayZoneProvider))
			if tc.hint == "" {
				require.Equal(t, tc.want == usagepolicy.ProviderLimitHard, clibridge.IsHardQuotaLimit(fmt.Errorf("wrapped: %w", pe)))
				require.Equal(t, tc.want == usagepolicy.ProviderLimitHard, isQuotaLimit(pe))
			}
		})
	}
	require.False(t, clibridge.IsHardQuotaLimit(errors.New("quota exhausted HTTP 429")))
	require.False(t, clibridge.IsHardQuotaLimit(nil))
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
				want := usagepolicy.ProviderLimitTransient
				if seconds >= 1800 {
					want = usagepolicy.ProviderLimitHard
				}
				require.Equal(t, want, usagepolicy.ClassifyHardProviderLimit(pe, "", now, usagepolicy.DisplayZoneProvider))
			})
		}
	}
	pe := &fantasy.ProviderError{StatusCode: 429, URL: "https://api.stepfun.ai/v1", Message: "quota", ResponseHeaders: map[string]string{"Retry-After": "1", "X-Ratelimit-Reset-Tokens": "30m"}, ResponseBody: []byte(`{"error":{"resets_in_seconds":1,"resets_at":1791462600}}`)}
	require.Equal(t, usagepolicy.ProviderLimitHard, usagepolicy.ClassifyHardProviderLimit(pe, "", now, usagepolicy.DisplayZoneProvider))
	reset, ok := usagepolicy.QuotaLimitResetTime(pe, now)
	require.True(t, ok)
	require.Equal(t, now.Add(time.Second), reset)
	pe.ResponseHeaders = nil
	pe.ResponseBody = []byte(fmt.Sprintf(`{"error":{"resets_in_seconds":1,"resets_at":%d}}`, now.Add(time.Hour).Unix()))
	require.Equal(t, usagepolicy.ProviderLimitHard, usagepolicy.ClassifyHardProviderLimit(pe, "", now, usagepolicy.DisplayZoneProvider))
	require.Equal(t, usagepolicy.ProviderLimitUnknown, usagepolicy.ClassifyHardProviderLimit(&fantasy.ProviderError{StatusCode: 503, ResponseHeaders: map[string]string{"Retry-After": "1800"}}, "", now, usagepolicy.DisplayZoneProvider))
	_, ok = usagepolicy.QuotaLimitResetTime(nil, now)
	require.False(t, ok)
}
