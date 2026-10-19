// Rate-limit (429 non-quota) wait budget for the coordinator retry loop.

package agent

import (
	"context"
	"errors"
	"math"
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

type rateLimitWaitObservationKey struct{}

type rateLimitWaitObservation func()

func withRateLimitWaitObservation(ctx context.Context, observe rateLimitWaitObservation) context.Context {
	return context.WithValue(ctx, rateLimitWaitObservationKey{}, observe)
}

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
	// Headers arrive canonical ("Retry-After") from net/http; match any case.
	get := func(name string) string {
		for k, v := range pe.ResponseHeaders {
			if strings.EqualFold(k, name) {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	// Two passes so both keys present is deterministic.
	if ms := get("retry-after-ms"); ms != "" {
		if n, e := strconv.ParseInt(ms, 10, 64); e == nil {
			if n > 0 {
				capMS := int64(rateLimitMaxWait / time.Millisecond)
				if n > capMS {
					return rateLimitMaxWait, true
				}
				return capRateLimitWait(time.Duration(n) * time.Millisecond), true
			}
		} else if isPositiveDecimalInteger(ms) {
			return rateLimitMaxWait, true
		}
	}
	if ra := get("retry-after"); ra != "" {
		if f, e := strconv.ParseFloat(ra, 64); e == nil {
			if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
				return 0, false
			}
			maxSeconds := float64(rateLimitMaxWait) / float64(time.Second)
			if f >= maxSeconds || f > float64(math.MaxInt64)/float64(time.Second) {
				return rateLimitMaxWait, true
			}
			return time.Duration(f * float64(time.Second)), true
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

func isPositiveDecimalInteger(s string) bool {
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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
	if wait >= rateLimitMaxWait || priorWaits <= 0 {
		return capRateLimitWait(wait)
	}
	for i := 0; i < priorWaits && wait < rateLimitMaxWait; i++ {
		if wait > rateLimitMaxWait/3 {
			return rateLimitMaxWait
		}
		wait *= 3
	}
	return capRateLimitWait(wait)
}

// hasLiveTreeWork reports whether sessionID (or any session in its running
// delegation tree) still has undelivered work in the ledger — signal that
// the operator is watching live output and the wait budget should pause.
func (c *coordinator) hasLiveTreeWork(sessionID string) bool {
	if facts := coordinatorBatchCutoverFacts(c); facts != nil {
		// Flag on: liveness is a batch live read (own roots plus live
		// delegation descendants, the R10b/R11G semantics). A FAILED read
		// answers not-live HERE, against the seam's conservative rule,
		// deliberately: this answer only decides whether the rate-limit
		// wait budget may consume, and "pause forever on an unreadable
		// source" made the 429 retry loop unbounded (F3 — the loop's only
		// other exit is ctx cancellation). Worst case the budget exhausts
		// and the run surfaces the ORIGINAL provider error; the durable
		// batch work itself is untouched. Durable consumers (drain policy,
		// CLI scope, supervision) keep their own conservative reads.
		own, descendants, err := facts.LiveWorkSummary(context.Background(), sessionID)
		if err != nil {
			return false
		}
		return own > 0 || descendants > 0
	}
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
		if observe, ok := ctx.Value(rateLimitWaitObservationKey{}).(rateLimitWaitObservation); ok {
			observe()
		}
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

// RateLimitNotifier is the optional coordinator extension the app installs
// to surface each provider rate-limit wait to the operator.
type RateLimitNotifier interface {
	SetRateLimitNotify(f func(sessionID string, until time.Time, attempt int))
}

// SetRateLimitNotify installs the per-wait callback.
func (c *coordinator) SetRateLimitNotify(f func(sessionID string, until time.Time, attempt int)) {
	if f == nil {
		return
	}
	c.rateLimitNotify.Store(&f)
}

// notifyRateLimitWait invokes the installed callback, if any.
func (c *coordinator) notifyRateLimitWait(sessionID string, until time.Time, attempt int) {
	if f := c.rateLimitNotify.Load(); f != nil {
		(*f)(sessionID, until, attempt)
	}
}
