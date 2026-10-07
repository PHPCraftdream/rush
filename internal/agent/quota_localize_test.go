package agent

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setTestLocal pins the reset display zone to UTC+2 for the test (not
// parallel-safe). time.Local itself is left alone: the process-wide heartbeat
// writer reads it through time.Now, so mutating it is a data race.
func setTestLocal(t *testing.T) {
	t.Helper()
	old := resetDisplayZone
	zone := time.FixedZone("TEST", 2*3600)
	resetDisplayZone = func() *time.Location { return zone }
	t.Cleanup(func() { resetDisplayZone = old })
}

func TestLocalizeResetHint(t *testing.T) {
	setTestLocal(t)
	// 12:00 UTC = 20:00 CST = 14:00 local.
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name, in, want string
	}{
		{
			"future reset gets local time and countdown",
			"retry error: too many requests: Usage limit reached for 5 hour. Your limit will reset at 2026-10-01 20:34:01",
			"retry error: too many requests: Usage limit reached for 5 hour. Your limit will reset at 2026-10-01 14:34:01 +02:00 (in 34m01s)",
		},
		{
			"past reset has no countdown",
			"Your limit will reset at 2026-10-01 19:00:00 tail",
			"Your limit will reset at 2026-10-01 13:00:00 +02:00 tail",
		},
		{
			"T separator, date rolls back across midnight",
			"Your limit will reset at 2026-10-02T01:30:00.",
			"Your limit will reset at 2026-10-01 19:30:00 +02:00 (in 5h30m00s).",
		},
		{"no hint", "something else failed", "something else failed"},
		{
			"already localized is unchanged",
			"Your limit will reset at 2026-10-01 14:34:01 +02:00 (in 34m01s)",
			"Your limit will reset at 2026-10-01 14:34:01 +02:00 (in 34m01s)",
		},
		{
			"explicit Z is respected as zone-qualified",
			"Your limit will reset at 2026-10-01 12:34:01Z",
			"Your limit will reset at 2026-10-01 12:34:01Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LocalizeResetHint(tc.in, now)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, got, LocalizeResetHint(got, now), "idempotent")
		})
	}
}

func TestParseZAIResetHint_QualifiedStampKeepsItsOffset(t *testing.T) {
	got, ok := parseZAIResetHint("Your limit will reset at 2026-10-01 14:34:01 +02:00 (in 1m)")
	require.True(t, ok)
	assert.True(t, got.Equal(time.Date(2026, 10, 1, 12, 34, 1, 0, time.UTC)))
}

func TestLocalizeResetError(t *testing.T) {
	setTestLocal(t)
	pe := &fantasy.ProviderError{StatusCode: 429, Message: "Usage limit reached. Your limit will reset at 2026-10-01 20:34:01"}
	wrapped := fmt.Errorf("retry error: %w", pe)

	got := LocalizeResetError(wrapped)
	assert.NotContains(t, got.Error(), "20:34:01")
	assert.Contains(t, got.Error(), "14:34:01 +02:00")
	var back *fantasy.ProviderError
	assert.True(t, errors.As(got, &back), "errors.As must still reach the provider error")
	assert.Same(t, pe, back)

	plain := errors.New("boom")
	assert.Same(t, plain, LocalizeResetError(plain))
	assert.NoError(t, LocalizeResetError(nil))
}

// TestRun_QuotaLimitFinishLocalizesInline: the finish shows the reset once,
// in local time, with the remote stamp gone.
func TestRun_QuotaLimitFinishLocalizesInline(t *testing.T) {
	setTestLocal(t)
	env := testEnv(t)
	agent := testSessionAgent(env, quotaLimitModel{}, quotaLimitModel{}, "test system prompt")

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)
	_, err = agent.Run(t.Context(), SessionAgentCall{Prompt: "hello", SessionID: sess.ID, MaxOutputTokens: 100})
	require.Error(t, err)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	var finish *message.Finish
	for i := range msgs {
		if msgs[i].Role == message.Assistant {
			finish = msgs[i].FinishPart()
		}
	}
	require.NotNil(t, finish)
	// 2026-06-17 14:49:28 CST == 08:49:28 +02:00.
	assert.Contains(t, finish.Details, "Your limit will reset at 2026-06-17 08:49:28 +02:00")
	assert.NotContains(t, finish.Details, "14:49:28")
	assert.NotContains(t, finish.Details, "Limit resets:", "one reset, shown once")
}
