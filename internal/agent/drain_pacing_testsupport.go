package agent

import "time"

// SetDrainPacingForTest shrinks the Drain launch pacing (the pause after an
// unreacted attempt, the pause after a refusal for a session this process's own
// `rush run` loop drives) and the 60s re-check interval, for tests in other
// packages that drive the real loop at test timescale. A non-positive value
// leaves that knob alone. The returned func restores the previous values. Never
// called in production.
func SetDrainPacingForTest(retryAfter, refusalPause, recheckInterval time.Duration) (restore func()) {
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
