package agent

import (
	"testing"
	"time"
)

func setSupervisionDefaultIntervalForTest(t *testing.T, d time.Duration) {
	t.Helper()
	if !testing.Testing() {
		panic("setSupervisionDefaultIntervalForTest called outside a test binary")
	}
	old := supervisionIntervalNS.Swap(int64(d))
	t.Cleanup(func() { supervisionIntervalNS.Store(old) })
}
