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
	c := newBatchRuntimeCoordinator()
	// hasLiveTreeWork answers from the bound batch facts, so the pause needs
	// a RUNNING batch root for "root".
	conn, dataDir := batchFixtureConn(t)
	bindBatchNodeFacts(t, c, conn, dataDir)
	seedBatchDelegation(t, conn, "root", "call-live", "", "command", "running", false)
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

// rateLimitFailedBatchFacts is a BOUND fact source whose liveness read fails
// at runtime: the shape nodeBatchCutoverFacts.LiveWorkSummary returns on a
// DB error, and unknownBatchCutoverFacts returns when the wiring is missing.
// Only LiveWorkSummary is reachable from the rate-limit budget path; the
// embedded interface keeps the stub honest about that scope.
type rateLimitFailedBatchFacts struct{ BatchCutoverFacts }

func (rateLimitFailedBatchFacts) LiveWorkSummary(context.Context, string) (int, int, error) {
	return 1, 1, ErrBatchFactsSourceUnavailable
}

// TestWaitRateLimit_FailingBatchFactsConsumesBudget pins the F3 bound: a
// failed batch liveness read must not pause the rate-limit budget, or the
// 429 retry loop never ends (the budget can never exhaust).
//
// Revert-check: restoring hasLiveTreeWork's `if err != nil { return true }`
// makes waitRateLimit sleep out the whole wait without consuming, and the
// errRateLimitBudgetExhausted assertion goes red.
func TestWaitRateLimit_FailingBatchFactsConsumesBudget(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	setRateLimitBudgetTick(t, 0, time.Millisecond)
	c := &coordinator{}
	BindCoordinatorBatchCutoverFacts(c, rateLimitFailedBatchFacts{})
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(c) })
	var spent time.Duration
	err := c.waitRateLimit(t.Context(), "s", 20*time.Millisecond, &spent)
	require.ErrorIs(t, err, errRateLimitBudgetExhausted,
		"a failed liveness read must let the budget run out, never pause it forever")
}
