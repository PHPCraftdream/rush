// The 60s recheck pass and its ticker lifecycle (docs/plans/2026-09-28-
// async-phase4-durable-core.md sec.3.4 rule (b)/sec.3.5, step 4 review). The
// refused-launch retry through the pass is covered by
// TestAdmissionRefusedRelease_NoImmediateRelaunch_ThenTickRecovers.
package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRecheckTicker_StopsCleanly_NoGoroutineLeak confirms StartRecheckTicker's
// goroutine actually exits once stopped (via CancelAll, production's own
// shutdown path) rather than leaking -- doc sec.3.5's 60s pass must not
// outlive the coordinator.
//
// REVERT CHECK: commented out the `c.StopRecheckTicker()` call inside
// CancelAll (coordinator_interrupt.go) -- this test's `<-done` select
// FAILED (timed out after 2s: the ticker goroutine was still alive, parked
// on its own ctx that CancelAll no longer cancelled). Restored the call
// (byte-identical diff confirmed); re-ran, passed.
func TestRecheckTicker_StopsCleanly_NoGoroutineLeak(t *testing.T) {
	coord := &coordinator{currentAgent: &mockSessionAgent{}}

	coord.StartRecheckTicker()
	coord.StartRecheckTicker() // idempotent: must not spawn a second goroutine
	require.NotNil(t, coord.recheckStop)
	done := coord.recheckDone
	require.NotNil(t, done)

	select {
	case <-done:
		t.Fatal("the ticker goroutine must not have exited before it was ever stopped")
	default:
	}

	coord.CancelAll() // production's shutdown path
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the ticker goroutine must exit once CancelAll stops it -- no leak")
	}

	// A second CancelAll (idempotent shutdown) must not panic or hang.
	coord.CancelAll()
}
