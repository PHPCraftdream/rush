// Supervision deadline interval growth: doubles the current interval,
// capped at the configured maximum. Moved from internal/agent
// (supervision.go) as an exact mixed declaration.
package watchdog

import "time"

// GrowInterval doubles cur, capped at max.
func GrowInterval(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		return max
	}
	return next
}
