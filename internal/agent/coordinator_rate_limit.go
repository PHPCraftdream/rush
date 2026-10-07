// Rate-limit (429 non-quota) wait budget for the coordinator retry loop.

package agent

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
)

// Exponential wait schedule: 10s x 3^n, n = prior rate-limit waits in this
// run. Vars, not consts, so tests can shrink them (see
// streamStallRetryBaseBackoff).
var (
	rateLimitBaseWait = 10 * time.Second
	rateLimitMaxWait  = 5 * time.Minute
	rateLimitBudget   = 30 * time.Minute
	rateLimitWaitTick = 500 * time.Millisecond
)

var errRateLimitBudgetExhausted = errors.New("coordinator: rate limit retry budget exhausted")

// isRateLimitError reports whether err is a 429 that is NOT a quota wall —
// the overload class that deserves its own wait budget. fantasy.RetryError
// unwraps to its last error, so errors.As reaches the ProviderError through
// it.
func isRateLimitError(err error) bool {
	var pe *fantasy.ProviderError
	return errors.As(err, &pe) &&
		pe.StatusCode == http.StatusTooManyRequests &&
		!isQuotaLimit(pe)
}

// retryAfter reads the provider's Retry-After hint off the ProviderError's
// captured headers: "retry-after-ms" (integer milliseconds) first, then
// "retry-after" (float seconds, or an HTTP-date). Any positive result is
// capped at rateLimitMaxWait.
func retryAfter(err error) (time.Duration, bool) {
	var pe *fantasy.ProviderError
	if !errors.As(err, &pe) || pe.ResponseHeaders == nil {
		return 0, false
	}
	get := func(name string) string {
		if v, ok := pe.ResponseHeaders[name]; ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	// Two passes so both keys present is deterministic.
	if ms := get("retry-after-ms"); ms != "" {
		if n, e := strconv.Atoi(ms); e == nil && n > 0 {
			return capRateLimitWait(time.Duration(n) * time.Millisecond), true
		}
	}
	if ra := get("retry-after"); ra != "" {
		if f, e := strconv.ParseFloat(ra, 64); e == nil && f > 0 {
			return capRateLimitWait(time.Duration(f * float64(time.Second))), true
		}
		if t, e := http.ParseTime(ra); e == nil {
			if d := time.Until(t); d > 0 {
				return capRateLimitWait(d), true
			}
			return 0, false
		}
	}
	return 0, false
}

// capRateLimitWait clamps any wait to the per-wait cap.
func capRateLimitWait(d time.Duration) time.Duration {
	if d > rateLimitMaxWait {
		return rateLimitMaxWait
	}
	return d
}

// rateLimitWaitFor picks the wait before the next attempt: the provider's
// Retry-After hint if present, else exponential (10s x 3^priorWaits),
// always capped.
func rateLimitWaitFor(err error, priorWaits int) time.Duration {
	if wait, ok := retryAfter(err); ok {
		return wait
	}
	wait := rateLimitBaseWait
	for i := 0; i < priorWaits; i++ {
		wait *= 3
	}
	return capRateLimitWait(wait)
}

// hasLiveTreeWork reports whether sessionID (or any session in its running
// delegation tree) still has undelivered work in the ledger — signal that
// the operator is watching live output and the wait budget should pause.
func (c *coordinator) hasLiveTreeWork(sessionID string) bool {
	if c.asyncJobs == nil {
		return false
	}
	for _, id := range c.asyncJobs.treeSessionIDs(sessionID) {
		if c.asyncJobs.running(id) {
			return true
		}
	}
	return false
}

// waitRateLimit sleeps out wait in tick-sized steps, returning early on ctx
// cancellation. While live tree work exists the budget does not consume;
// once it stops, remaining budget below rateLimitBudget is all that's left,
// and exhausting it fails with errRateLimitBudgetExhausted.
func (c *coordinator) waitRateLimit(ctx context.Context, sessionID string, wait time.Duration, spent *time.Duration) error {
	remaining := wait
	for remaining > 0 {
		live := c.hasLiveTreeWork(sessionID)
		if !live && *spent >= rateLimitBudget {
			return errRateLimitBudgetExhausted
		}
		step := rateLimitWaitTick
		if step > remaining {
			step = remaining
		}
		if !live && rateLimitBudget-*spent < step {
			step = rateLimitBudget - *spent
		}
		timer := time.NewTimer(step)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if !live {
			*spent += step
		}
		remaining -= step
	}
	return nil
}
