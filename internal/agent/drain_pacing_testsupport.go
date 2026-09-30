package agent

import (
	"testing"
	"time"
)

// SetDrainPacingForTest shrinks the Drain launch pacing (the pause after an
// unreacted attempt, the pause after a refusal for a session this process's own
// `rush run` loop drives) and the 60s re-check interval, for tests in other
// packages that drive the real loop at test timescale (`internal/app` cannot
// reach this package's _test.go files). A non-positive value leaves that knob
// alone. The returned func restores the previous values.
//
// It panics outside a test binary (testing.Testing()), so production code can
// never change the pacing; the knobs themselves stay unexported atomics.
func SetDrainPacingForTest(retryAfter, refusalPause, recheckInterval time.Duration) (restore func()) {
	if !testing.Testing() {
		panic("agent.SetDrainPacingForTest called outside a test binary")
	}
	oldRetry, oldRefusal, oldRecheck := drainRetryAfterNS.Load(), drainRefusalPauseNS.Load(), recheckPassIntervalNS.Load()
	if retryAfter > 0 {
		drainRetryAfterNS.Store(int64(retryAfter))
	}
	if refusalPause > 0 {
		drainRefusalPauseNS.Store(int64(refusalPause))
	}
	if recheckInterval > 0 {
		recheckPassIntervalNS.Store(int64(recheckInterval))
	}
	return func() {
		drainRetryAfterNS.Store(oldRetry)
		drainRefusalPauseNS.Store(oldRefusal)
		recheckPassIntervalNS.Store(oldRecheck)
	}
}
