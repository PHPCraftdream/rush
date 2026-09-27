package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSubAgentWorkTerminal_DriverBusyIsNotTerminal pins task #1049 fix item
// 3: subAgentWorkTerminal must consult the child's REGISTERED DRIVER, not
// c.currentAgent, for its busy gate. Before the fix, a delegated child's
// driver being mid-turn was invisible to this check -- c.currentAgent (the
// root/interactive coder agent) is always a DIFFERENT SessionAgent than the
// one actually running a delegated child's turns, so it reported "idle" for
// a child it never touched, and a release could fire while the real driver
// was still mid-stream, delivering the child's pre-result text.
func TestSubAgentWorkTerminal_DriverBusyIsNotTerminal(t *testing.T) {
	t.Parallel()
	coord := &coordinator{}
	coord.asyncJobs = newAsyncJobRegistry(nil)
	coord.subAgentOutcomes = newSubAgentOutcomeRegistry(coord)
	coord.subAgentDrivers = newSubAgentDriverRegistry()

	// c.currentAgent, if wrongly consulted for a driven child, would report
	// idle: mockSessionAgent's IsSessionBusy is always false, and it is never
	// the SessionAgent a delegated child actually runs on.
	coord.currentAgent = &mockSessionAgent{}

	driverStub := newBusyStubAgent()
	driverStub.setBusy(true)
	coord.subAgentDrivers.register("child-1", subAgentDriver{agent: driverStub})

	require.False(t, coord.subAgentWorkTerminal("child-1"),
		"a busy driver must hold the child non-terminal even when c.currentAgent reports idle")

	driverStub.setBusy(false)
	require.True(t, coord.subAgentWorkTerminal("child-1"),
		"once the driver is idle (and no async/background work is pending) the child is terminal")

	// A session with no registered driver falls back to c.currentAgent,
	// preserving behavior for non-delegated sessions and bare test fixtures.
	require.True(t, coord.subAgentWorkTerminal("no-driver-session"),
		"a session with no registered driver must fall back to c.currentAgent's busy state")
}
