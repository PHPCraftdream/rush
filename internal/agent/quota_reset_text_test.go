package agent

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// External Claude wording, not reproduced locally: docs/research/2026-10-08-provider-limit-errors.md, anthropics/claude-code #2087, #9236, #19673.
const (
	cliClaudeClockFixture = "You've hit your limit · resets 4pm (Asia/Kuala_Lumpur)"
	// External Codex wording, not reproduced locally: docs/research/2026-10-08-provider-limit-errors.md, agent-grounds/rhei #471, zhupanov/larch #3380.
	cliCodexResetFixture = "You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Oct 9th, 2026 11:19 PM."
)

// Revert-check: quota_reset_text.go:parseCLIResetText must parse all three hint families.
func TestCLIResetText(t *testing.T) {
	now := time.Date(2026, 10, 9, 15, 0, 0, 0, time.Local)
	date := func(day, hour, minute int) time.Time { return time.Date(2026, 10, day, hour, minute, 0, 0, time.Local) }
	for _, tc := range []struct {
		text string
		want time.Time
	}{
		{"Claude AI usage limit reached|1749924000", time.Unix(1749924000, 0)},
		{cliCodexResetFixture, date(9, 23, 19)},
		{"try again at Oct 1st, 2026 10:27 AM", date(1, 10, 27)},
		{"try again at Oct 2nd, 2026 12:00 AM", date(2, 0, 0)},
		{"try again at Oct 3rd, 2026 12:00 PM", date(3, 12, 0)},
		{"try again at Oct 9th, 2026 11:19 PM", date(9, 23, 19)},
		{"limit will reset at 4pm", date(9, 16, 0)},
		{"resets 3pm", date(10, 15, 0)},
		{"resets 2:59pm", date(10, 14, 59)},
		{"resets 12am", date(10, 0, 0)},
		{"resets 12pm", date(10, 12, 0)},
		{"resets 4:01PM (Unknown/Zone)", date(9, 16, 1)},
	} {
		t.Run(tc.text, func(t *testing.T) {
			got, ok := parseCLIResetText(tc.text, now)
			require.True(t, ok)
			require.True(t, tc.want.Equal(got), "want=%s got=%s", tc.want, got)
		})
	}
	zone, err := time.LoadLocation("Asia/Kuala_Lumpur")
	require.NoError(t, err)
	zonedNow := time.Date(2026, 12, 31, 16, 0, 0, 0, zone)
	got, ok := parseCLIResetText(cliClaudeClockFixture, zonedNow)
	require.True(t, ok)
	require.True(t, time.Date(2027, 1, 1, 16, 0, 0, 0, zone).Equal(got))
	// External Claude wording, not reproduced locally: docs/research/2026-10-08-provider-limit-errors.md, anthropics/claude-code #9236, #19673; user-supplied timezone variant.
	newYork, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	for _, hour := range []int{14, 15, 16} {
		fixedNow := time.Date(2026, 10, 9, hour, 0, 0, 0, newYork)
		wantDay := 9
		if hour >= 15 {
			wantDay = 10
		}
		got, ok := parseCLIResetText("Claude usage limit reached. Your limit will reset at 3pm (America/New_York)", fixedNow)
		require.True(t, ok)
		require.True(t, time.Date(2026, 10, wantDay, 15, 0, 0, 0, newYork).Equal(got))
		require.True(t, got.After(fixedNow))
	}
}

// Revert-check: quota_reset_text.go:parseCLIResetText rejects malformed whole tokens.
func TestCLIResetTextMalformed(t *testing.T) {
	now := time.Date(2026, 10, 9, 15, 0, 0, 0, time.Local)
	for _, text := range []string{"", "garbage 4pm", "resets 0pm", "resets 13pm", "resets 4:60pm", "resets 4:1pm", "resets 4pmjunk", "resets 4pm (Asia/Kuala_Lumpur", "resets 4pm (Asia/Kuala_Lumpur)junk", "resets 4pm ()", "resets 4pm (bad zone)", "resets 4pmm", "reset at 44pm", "resets -1pm", "resets 4pm:30", "resets 4pm UTC", "try again at Oct 32nd, 2026 11:19 PM", "try again at Feb 29th, 2026 11:19 PM", "try again at Oct 9th, 2026 13:19 PM", "try again at Oct 9th, 2026 0:19 PM", "try again at Oct 9th, 2026 11:1 PM", "try again at Oct 9th, 2026 11:99 PM", "try again at Oct 9th, 2026 11:19 PMjunk", "usage limit reached|0", "usage limit reached|-1", "usage limit reached|1749924000x", "usage limit reached|1749924000000", "usage limit reached|999999999999999999999999"} {
		t.Run(text, func(t *testing.T) { _, ok := parseCLIResetText(text, now); require.False(t, ok) })
	}
}

// Revert-check: quota_reset.go:QuotaLimitResetTime's !ok deferred fallback exposes CLI hints.
func TestCLIResetHook(t *testing.T) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.Local)
	for _, text := range []string{cliClaudeClockFixture, cliCodexResetFixture, "Claude AI usage limit reached|1749924000"} {
		expected, ok := parseCLIResetText(text, now)
		require.True(t, ok)
		pe := &fantasy.ProviderError{StatusCode: 429, Title: "Quota limit", Message: text}
		require.True(t, IsHardQuotaLimit(pe))
		for _, err := range []error{pe, fmt.Errorf("wrapped: %w", pe), errors.New(text), fmt.Errorf("wrapped: %w", errors.New(text)), &fantasy.ProviderError{StatusCode: 429, Message: text, ResponseHeaders: map[string]string{}}} {
			reset, found := QuotaLimitResetTime(err, now)
			require.True(t, found, "%v", err)
			require.True(t, expected.Equal(reset), "%v: want=%s got=%s", err, expected, reset)
		}
	}
	_, ok := QuotaLimitResetTime(nil, now)
	require.False(t, ok)
}

// Revert-check: quota_reset.go:QuotaLimitResetTime's fallback must not replace successful existing parsers.
func TestCLIResetPrecedence(t *testing.T) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.Local)
	for _, headers := range []map[string]string{nil, {}, {"Retry-After": "30"}} {
		pe := &fantasy.ProviderError{StatusCode: 429, Message: cliCodexResetFixture, ResponseHeaders: headers, ResponseBody: []byte(`{"error":{"resets_in_seconds":60}}`)}
		want := now.Add(time.Minute)
		if len(headers) > 0 {
			want = now.Add(30 * time.Second)
		}
		got, ok := QuotaLimitResetTime(pe, now)
		require.True(t, ok)
		require.True(t, want.Equal(got), "want=%s got=%s", want, got)
	}
	pe := &fantasy.ProviderError{StatusCode: 429, Message: "Your limit will reset at 2026-10-09 14:49:28\n" + cliCodexResetFixture}
	want, ok := parseZAIResetHint(pe.Message)
	require.True(t, ok)
	got, ok := QuotaLimitResetTime(pe, now)
	require.True(t, ok)
	require.True(t, want.Equal(got))
}
