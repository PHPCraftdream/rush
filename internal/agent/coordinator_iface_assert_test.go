package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The optional interfaces the server, cmd and app layers reach through a
// Coordinator VALUE (type assertions, which compile whatever the signature)
// are really implemented by the real coordinator. The compile-time half is
// coordinator_iface_assert.go; this is the same assertion the callers make.
//
// Revert-check: renaming or re-typing any interface method (or the matching
// *coordinator method) breaks the build through coordinator_iface_assert.go
// and turns an assertion here red.
func TestCoordinatorOptionalInterfaces_ReachableThroughCoordinatorValue(t *testing.T) {
	var c Coordinator = &coordinator{}
	_, ok := c.(AutoTurnHolder)
	require.True(t, ok, "the server's rerun hold reaches the coordinator through AutoTurnHolder")
	_, ok = c.(ReactionDebtSource)
	require.True(t, ok, "the rush run loop reaches the coordinator through ReactionDebtSource")
	_, ok = c.(ParkedSubAgentWorkReporter)
	require.True(t, ok, "sessions list reaches the coordinator through ParkedSubAgentWorkReporter")
}

// SetDrainPacingForTest keeps its name and signature (internal/app tests call
// it) and restores what it changed. Its panic outside a test binary
// (testing.Testing()) cannot be exercised from inside one.
//
// Revert-check: dropping the restore of any knob leaves it changed and this
// test goes red.
func TestSetDrainPacingForTest_ShrinksAndRestores(t *testing.T) {
	oldRetry, oldRefusal, oldRecheck := drainRetryAfterNS.Load(), drainRefusalPauseNS.Load(), recheckPassIntervalNS.Load()

	restore := SetDrainPacingForTest(7*time.Millisecond, 0, 9*time.Millisecond)
	require.Equal(t, 7*time.Millisecond, drainRetryAfterFailure())
	require.EqualValues(t, oldRefusal, drainRefusalPauseNS.Load(), "a non-positive value leaves the knob alone")
	require.EqualValues(t, 9*time.Millisecond, recheckPassIntervalNS.Load())

	restore()
	require.EqualValues(t, oldRetry, drainRetryAfterNS.Load())
	require.EqualValues(t, oldRefusal, drainRefusalPauseNS.Load())
	require.EqualValues(t, oldRecheck, recheckPassIntervalNS.Load())
}
