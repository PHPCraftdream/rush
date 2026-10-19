// Run-scoped, CLI-origin-gated rate-limit diagnostic sink: replaces the
// app-wide process-stderr callback with a context-carried writer attached
// per run in app.prepareExecuteRun.

package agent

import (
	"context"
	"fmt"
	"io"
	"time"
)

// rateLimitNotifySinkKey is the context key for the per-run diagnostic
// sink. Unexported: only WithRateLimitNotifySink can set it.
type rateLimitNotifySinkKey struct{}

// WithRateLimitNotifySink attaches the run's diagnostic writer to the
// turn context. A nil writer is a no-op so callers can gate on their own
// predicate without a second branch here.
func WithRateLimitNotifySink(ctx context.Context, w io.Writer) context.Context {
	if w == nil {
		return ctx
	}
	return context.WithValue(ctx, rateLimitNotifySinkKey{}, w)
}

// RateLimitNotifySinkFrom returns the run-scoped rate-limit diagnostic
// sink, or nil when this run carries none (non-CLI origins, library
// callers, direct coordinator use).
func RateLimitNotifySinkFrom(ctx context.Context) io.Writer {
	if ctx == nil {
		return nil
	}
	w, _ := ctx.Value(rateLimitNotifySinkKey{}).(io.Writer)
	return w
}

// notifyRateLimitWaitContext reports one provider rate-limit wait to the
// run's own sink when it has one, falling back to the legacy app-installed
// callback otherwise. Same line format the removed app-wide callback wrote.
func (c *coordinator) notifyRateLimitWaitContext(ctx context.Context, sessionID string, until time.Time, attempt int) {
	if w := RateLimitNotifySinkFrom(ctx); w != nil {
		fmt.Fprintf(w, "rush run: session %q waiting for provider rate limit until %s (attempt %d)\n",
			sessionID, until.Format("15:04"), attempt)
		return
	}
	c.notifyRateLimitWait(sessionID, until, attempt)
}
