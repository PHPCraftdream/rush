package agent

import (
	"testing"
	"time"
)

// SetSupervisionDefaultIntervalForTest shrinks the built-in supervision
// check-in interval (5 minutes) for tests in other packages that drive the real
// `rush run` loop at test timescale. d must be at least the one-second floor
// (a smaller value is ignored: the default stands). The returned func restores the
// previous value. It panics outside a test binary (testing.Testing()).
func SetSupervisionDefaultIntervalForTest(d time.Duration) (restore func()) {
	if !testing.Testing() {
		panic("agent.SetSupervisionDefaultIntervalForTest called outside a test binary")
	}
	old := supervisionIntervalNS.Swap(int64(d))
	return func() { supervisionIntervalNS.Store(old) }
}
