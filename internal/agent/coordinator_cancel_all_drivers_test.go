// B18 (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN):
// coordinator.CancelAll must reach every registered delegation driver, not
// only c.currentAgent -- a delegated child's driver is a SEPARATE
// SessionAgent (task #1049), so before this fix its own in-flight Drain
// turn (built on context.Background()) survived process shutdown entirely.
package agent

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// cancelTrackingAgent wraps a *mockSessionAgent and overrides ONLY CancelAll
// to make it observable (the shared mockSessionAgent.CancelAll is a fixed
// no-op returning false, used by many other tests that don't care about
// this specific call).
type cancelTrackingAgent struct {
	*mockSessionAgent
	cancelAllCalls atomic.Int32
	stillBusy      bool
}

func (c *cancelTrackingAgent) CancelAll() bool {
	c.cancelAllCalls.Add(1)
	return c.stillBusy
}

// TestCancelAll_ReachesRegisteredDelegationDrivers is the direct proof.
//
// Revert-check performed: reverted coordinator.CancelAll (coordinator_
// interrupt.go) to `return c.currentAgent.CancelAll()` (dropping the driver
// loop) -- this test's `require.EqualValues(t, 1, driver.cancelAllCalls.Load())`
// FAILED (0 calls: the registered driver was never reached). Restored the
// loop; re-ran, passed.
func TestCancelAll_ReachesRegisteredDelegationDrivers(t *testing.T) {
	coord := &coordinator{
		currentAgent:    &mockSessionAgent{},
		subAgentDrivers: newSubAgentDriverRegistry(),
	}
	driver := &cancelTrackingAgent{mockSessionAgent: &mockSessionAgent{}}
	coord.subAgentDrivers.register("child-session", subAgentDriver{agent: driver})

	coord.CancelAll()

	require.EqualValues(t, 1, driver.cancelAllCalls.Load(),
		"CancelAll must reach a registered delegation driver's own SessionAgent, not only currentAgent")
}

// TestCancelAll_StillBusyIsTrueIfAnyDriverIsStillBusy proves the aggregate
// stillBusy return value reflects a driver's own busy state too, not only
// currentAgent's.
func TestCancelAll_StillBusyIsTrueIfAnyDriverIsStillBusy(t *testing.T) {
	coord := &coordinator{
		currentAgent:    &mockSessionAgent{},
		subAgentDrivers: newSubAgentDriverRegistry(),
	}
	driver := &cancelTrackingAgent{mockSessionAgent: &mockSessionAgent{}, stillBusy: true}
	coord.subAgentDrivers.register("child-session", subAgentDriver{agent: driver})

	require.True(t, coord.CancelAll(), "a still-busy driver must make CancelAll's own aggregate result true")
}

// TestCancelAll_DedupesSharedDriverAgent guards allDriverAgents' dedup: two
// child ids registered against the SAME underlying agent must only be
// cancelled once.
func TestCancelAll_DedupesSharedDriverAgent(t *testing.T) {
	coord := &coordinator{
		currentAgent:    &mockSessionAgent{},
		subAgentDrivers: newSubAgentDriverRegistry(),
	}
	driver := &cancelTrackingAgent{mockSessionAgent: &mockSessionAgent{}}
	coord.subAgentDrivers.register("child-a", subAgentDriver{agent: driver})
	coord.subAgentDrivers.register("child-b", subAgentDriver{agent: driver})

	coord.CancelAll()

	require.EqualValues(t, 1, driver.cancelAllCalls.Load(), "a shared driver agent must be cancelled exactly once")
}
