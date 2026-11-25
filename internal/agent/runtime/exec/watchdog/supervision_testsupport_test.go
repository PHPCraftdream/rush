package watchdog

import (
	"sync/atomic"
	"testing"
	"time"
)

// SetSupervisionDefaultIntervalForTest overrides the supervision interval
// stored by the caller for tests and returns a function that restores it.
func SetSupervisionDefaultIntervalForTest(interval *atomic.Int64, d time.Duration) (restore func()) {
	if !testing.Testing() {
		panic("watchdog.SetSupervisionDefaultIntervalForTest called outside a test binary")
	}
	old := interval.Swap(int64(d))
	return func() { interval.Store(old) }
}
