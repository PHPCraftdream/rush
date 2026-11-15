package heartbeat

import (
	"time"
)

// SetRateLimitedUntilFor stamps a session's row with the pending
// rate-limit wait deadline; a zero time clears it. No row for the
// session is a silent no-op.
func SetRateLimitedUntilFor(sessionID string, until time.Time) {
	registry.Lock()
	defer registry.Unlock()
	stamp := ""
	if !until.IsZero() {
		stamp = until.UTC().Format(time.RFC3339)
	}
	for _, r := range registry.rows {
		if r.Session != sessionID {
			continue
		}
		r.RateLimitedUntil = stamp
		r.dirty = true
		r.generation++
	}
}
