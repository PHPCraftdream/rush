package agent

// REVERT-CHECKS:
//   - TestWaitRateLimitConsumesBudget: dropping `*spent += step` in
//     waitRateLimit (the budget would never run out).
//   - TestWaitRateLimitPausesBudgetWhileTreeWorkIsLive: replacing the
//     hasLiveTreeWork call with false (live workers would not hold the run).
//   - TestRateLimitWaitForPrefersRetryAfter: dropping the retryAfter branch of
//     rateLimitWaitFor.

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func setRateLimitBudgetTick(t *testing.T, budget, tick time.Duration) {
	t.Helper()
	prevBudget, prevTick := rateLimitBudget, rateLimitWaitTick
	rateLimitBudget, rateLimitWaitTick = budget, tick
	t.Cleanup(func() { rateLimitBudget, rateLimitWaitTick = prevBudget, prevTick })
}

func TestWaitRateLimitConsumesBudget(t *testing.T) {
	setRateLimitBudgetTick(t, 50*time.Millisecond, 5*time.Millisecond)
	c := &coordinator{}
	var spent time.Duration
	require.NoError(t, c.waitRateLimit(t.Context(), "s", 20*time.Millisecond, &spent))
	require.Equal(t, 20*time.Millisecond, spent)
	err := c.waitRateLimit(t.Context(), "s", time.Second, &spent)
	require.ErrorIs(t, err, errRateLimitBudgetExhausted, "the budget must run out")
}

func TestWaitRateLimitPausesBudgetWhileTreeWorkIsLive(t *testing.T) {
	setRateLimitBudgetTick(t, 0, 5*time.Millisecond)
	ledger := newWorkLedger(nil)
	ledger.bySession["root"] = &sessionJobs{jobs: map[string]*asyncJob{"job-1": {}}}
	c := &coordinator{asyncJobs: ledger}
	var spent time.Duration
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, c.waitRateLimit(ctx, "root", 30*time.Millisecond, &spent),
		"an exhausted budget must not end the wait while the run's own work is live")
	require.Zero(t, spent)
}

func TestRateLimitWaitForPrefersRetryAfter(t *testing.T) {
	err := &fantasy.ProviderError{StatusCode: 429, ResponseHeaders: map[string]string{"retry-after": "2"}}
	require.Equal(t, 2*time.Second, rateLimitWaitFor(err, 3), "Retry-After wins over the exponential schedule")
}
