package agent

import (
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQuotaLimitResetTime is the former internal/cmd's
// TestPingRateLimitReset, moved with pingRateLimitReset itself (task #979)
// -- rush run's quota-exceeded turn failure now reuses this exact parsing,
// so the implementation and its tests live together here.
func TestQuotaLimitResetTime(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	t.Run("retry-after seconds", func(t *testing.T) {
		t.Parallel()
		err := &fantasy.ProviderError{
			StatusCode:      429,
			ResponseHeaders: map[string]string{"Retry-After": "30"},
		}
		got, ok := QuotaLimitResetTime(err, now)
		require.True(t, ok)
		require.Equal(t, now.Add(30*time.Second), got)
	})

	t.Run("anthropic reset picks the latest bucket", func(t *testing.T) {
		t.Parallel()
		err := &fantasy.ProviderError{
			StatusCode: 429,
			ResponseHeaders: map[string]string{
				"Anthropic-Ratelimit-Requests-Reset":     "2026-06-02T12:00:10Z",
				"Anthropic-Ratelimit-Input-Tokens-Reset": "2026-06-02T12:00:45Z",
				"Anthropic-Ratelimit-Tokens-Reset":       "2026-06-02T12:00:20Z",
			},
		}
		got, ok := QuotaLimitResetTime(err, now)
		require.True(t, ok)
		require.Equal(t, time.Date(2026, 6, 2, 12, 0, 45, 0, time.UTC), got.UTC())
	})

	t.Run("retry-after wins over reset headers", func(t *testing.T) {
		t.Parallel()
		err := &fantasy.ProviderError{
			ResponseHeaders: map[string]string{
				"Retry-After":                        "5",
				"Anthropic-Ratelimit-Requests-Reset": "2026-06-02T13:00:00Z",
			},
		}
		got, ok := QuotaLimitResetTime(err, now)
		require.True(t, ok)
		require.Equal(t, now.Add(5*time.Second), got)
	})

	t.Run("openai duration headers", func(t *testing.T) {
		t.Parallel()
		err := &fantasy.ProviderError{
			ResponseHeaders: map[string]string{
				"X-Ratelimit-Reset-Requests": "1s",
				"X-Ratelimit-Reset-Tokens":   "6m0s",
			},
		}
		got, ok := QuotaLimitResetTime(err, now)
		require.True(t, ok)
		require.Equal(t, now.Add(6*time.Minute), got)
	})

	t.Run("non-provider error", func(t *testing.T) {
		t.Parallel()
		_, ok := QuotaLimitResetTime(errors.New("boom"), now)
		require.False(t, ok)
	})

	t.Run("provider error without reset hints", func(t *testing.T) {
		t.Parallel()
		err := &fantasy.ProviderError{StatusCode: 500, ResponseHeaders: map[string]string{"X-Foo": "bar"}}
		_, ok := QuotaLimitResetTime(err, now)
		require.False(t, ok)
	})

	t.Run("z.ai text-body fallback, no response headers", func(t *testing.T) {
		t.Parallel()
		err := &fantasy.ProviderError{
			StatusCode: 429,
			Message:    "Usage limit reached. Your limit will reset at 2026-06-17 14:49:28",
		}
		got, ok := QuotaLimitResetTime(err, now)
		require.True(t, ok)
		// No tz marker in the z.ai text -- parsed as a fixed CST (UTC+8) wall clock.
		require.Equal(t, time.Date(2026, 6, 17, 14, 49, 28, 0, time.FixedZone("CST", 8*3600)), got)
	})

	t.Run("z.ai text-body fallback with response headers present but no matching hint", func(t *testing.T) {
		t.Parallel()
		err := &fantasy.ProviderError{
			StatusCode:      429,
			ResponseHeaders: map[string]string{"X-Foo": "bar"},
			Message:         "Usage limit reached. Your limit will reset at 2026-06-17 14:49:28",
		}
		got, ok := QuotaLimitResetTime(err, now)
		require.True(t, ok)
		require.Equal(t, time.Date(2026, 6, 17, 14, 49, 28, 0, time.FixedZone("CST", 8*3600)), got)
	})
}

// TestQuotaLimitGuidance covers the acceptance criteria from task #979: a
// quota-classified error with a parseable reset time produces a message
// containing a local-time-formatted timestamp, and every other case
// degrades gracefully (ok=false, empty string, never a panic).
func TestQuotaLimitGuidance(t *testing.T) {
	t.Parallel()

	t.Run("quota wall with a parseable reset time produces a local-time line", func(t *testing.T) {
		t.Parallel()
		err := &fantasy.ProviderError{
			StatusCode: 429,
			Message:    "Usage limit reached for 5 hour. Your limit will reset at 2026-06-17 14:49:28",
		}
		guidance, ok := QuotaLimitGuidance(err)
		require.True(t, ok)
		assert.Contains(t, guidance, "Limit resets:")
		assert.Contains(t, guidance, "RFC3339:")
		// The formatted line must carry the LOCAL offset marker, not a bare
		// "Z"/UTC stamp -- this is the whole point of the fix (task #979:
		// "выводится в удалённом времени").
		utc := time.Date(2026, 6, 17, 14, 49, 28, 0, time.FixedZone("CST", 8*3600)).Local()
		assert.Contains(t, guidance, utc.Format("2006-01-02 15:04:05 -07:00"))
	})

	t.Run("not a quota error at all (403) returns ok=false", func(t *testing.T) {
		t.Parallel()
		err := &fantasy.ProviderError{StatusCode: 403, Message: "forbidden"}
		guidance, ok := QuotaLimitGuidance(err)
		assert.False(t, ok)
		assert.Empty(t, guidance)
	})

	t.Run("quota-shaped message but no parseable reset time degrades gracefully", func(t *testing.T) {
		t.Parallel()
		// Matches isQuotaLimit's "quota" substring but carries nothing
		// QuotaLimitResetTime can parse -- must not panic, must report
		// ok=false so the caller falls back to the raw message.
		err := &fantasy.ProviderError{StatusCode: 429, Message: "You exceeded your quota, contact support"}
		guidance, ok := QuotaLimitGuidance(err)
		assert.False(t, ok)
		assert.Empty(t, guidance)
	})

	t.Run("plain non-provider error returns ok=false without panicking", func(t *testing.T) {
		t.Parallel()
		guidance, ok := QuotaLimitGuidance(errors.New("boom"))
		assert.False(t, ok)
		assert.Empty(t, guidance)
	})

	t.Run("momentary overload (429 but not a quota wall) returns ok=false", func(t *testing.T) {
		t.Parallel()
		err := &fantasy.ProviderError{
			StatusCode: 429,
			Message:    "The service may be temporarily overloaded",
			ResponseHeaders: map[string]string{
				"Retry-After": "5",
			},
		}
		guidance, ok := QuotaLimitGuidance(err)
		assert.False(t, ok, "isQuotaLimit must reject a momentary-overload 429 even with a parseable Retry-After")
		assert.Empty(t, guidance)
	})
}
