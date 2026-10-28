package agent

import (
	"testing"
	"time"
)

// SetTurnRetryBackoffForTest shrinks the coordinator's first backoff before it
// retries a transient turn failure (10s in production), for tests in other
// packages that drive a real turn through a failing provider stub
// (`internal/app` cannot reach this package's _test.go files). The returned
// func restores the previous value; call it only after the App built under the
// shrunken value has shut down.
//
// It panics outside a test binary (testing.Testing()), so production code can
// never change the backoff.
func SetTurnRetryBackoffForTest(base time.Duration) (restore func()) {
	if !testing.Testing() {
		panic("agent.SetTurnRetryBackoffForTest called outside a test binary")
	}
	old := streamStallRetryBaseBackoff
	streamStallRetryBaseBackoff = base
	return func() { streamStallRetryBaseBackoff = old }
}
