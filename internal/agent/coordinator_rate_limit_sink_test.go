// Run-scoped rate-limit diagnostic sink (rate_limit_notify_ctx.go): a run
// whose context carries a sink gets the wait diagnostic on that sink only;
// a run without a sink falls back to the legacy installed callback; with
// neither, nothing is written anywhere.

package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// The literal the notifier writes. Independent of production code: the
// production format string must match this byte for byte.
const wantRateLimitLine = "rush run: session %q waiting for provider rate limit until %s (attempt %d)\n"

// TestRunInternal_RateLimitDiagGoesToRunSink: a run whose context carries a
// sink gets the per-wait diagnostic on THAT sink; the legacy callback path
// (the stand-in for the process stderr) stays untouched.
//
// Revert check: reverting the runInternal call site to
// notifyRateLimitWait (callback only) leaves the sink empty and this test
// red.
func TestRunInternal_RateLimitDiagGoesToRunSink(t *testing.T) {
	shrinkRateLimitVars(t, time.Millisecond, 10*time.Millisecond, time.Second, time.Millisecond, time.Millisecond)
	rlErr := rateLimit429Err()
	var sink, fallback lockedBuffer
	_, calls, _, err := rateLimitLoopRunWith(t, WithRateLimitNotifySink(t.Context(), &sink), "test-rl-sink", nil,
		func(c *coordinator) {
			c.SetRateLimitNotify(func(sessionID string, until time.Time, attempt int) {
				fmt.Fprintf(&fallback, wantRateLimitLine, sessionID, until.Format("15:04"), attempt)
			})
		},
		func(t *testing.T, env *fakeEnv, sess session.Session, ctx context.Context, n int, call SessionAgentCall) (*fantasy.AgentResult, error) {
			if n < 2 {
				return rateLimitErrorAttempt(t, env, sess, ctx, call, rlErr)
			}
			return &fantasy.AgentResult{}, nil
		})
	require.NoError(t, err)
	require.Equal(t, 3, calls)
	sink.requireExactly(t, 2)
	require.Empty(t, fallback.String(), "the legacy callback must not fire when a run sink is attached")
}

// TestRunInternal_RateLimitDiagFallsBackToCallback: no sink on the context
// means the legacy installed callback still receives the diagnostic (and
// with neither installed, nothing is written).
//
// Revert check: deleting the fallback call in notifyRateLimitWaitContext
// leaves the callback buffer empty and this test red.
func TestRunInternal_RateLimitDiagFallsBackToCallback(t *testing.T) {
	shrinkRateLimitVars(t, time.Millisecond, 10*time.Millisecond, time.Second, time.Millisecond, time.Millisecond)
	rlErr := rateLimit429Err()
	var fallback lockedBuffer
	_, calls, _, err := rateLimitLoopRunWith(t, t.Context(), "test-rl-fallback", nil,
		func(c *coordinator) {
			c.SetRateLimitNotify(func(sessionID string, until time.Time, attempt int) {
				fmt.Fprintf(&fallback, wantRateLimitLine, sessionID, until.Format("15:04"), attempt)
			})
		},
		func(t *testing.T, env *fakeEnv, sess session.Session, ctx context.Context, n int, call SessionAgentCall) (*fantasy.AgentResult, error) {
			if n < 2 {
				return rateLimitErrorAttempt(t, env, sess, ctx, call, rlErr)
			}
			return &fantasy.AgentResult{}, nil
		})
	require.NoError(t, err)
	require.Equal(t, 3, calls)
	fallback.requireExactly(t, 2)
}

// requireExactly asserts the buffer (lockedBuffer, declared in
// stream_watchdog_handlefire_test.go) holds exactly n diagnostic lines and
// nothing else.
func (b *lockedBuffer) requireExactly(t *testing.T, n int) {
	t.Helper()
	got := b.String()
	require.Equal(t, n, strings.Count(got, "waiting for provider rate limit until"), "unexpected diagnostic content: %q", got)
}
