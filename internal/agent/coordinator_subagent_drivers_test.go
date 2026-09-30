package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
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
	coord.asyncJobs = newWorkLedger(nil)
	coord.asyncJobs.store = newTestAsyncJobStore(t)
	coord.asyncJobs.coord = coord
	coord.subAgentDrivers = newSubAgentDriverRegistry()

	// c.currentAgent, if wrongly consulted for a driven child, would report
	// idle: mockSessionAgent's IsSessionBusy is always false, and it is never
	// the SessionAgent a delegated child actually runs on.
	coord.currentAgent = &mockSessionAgent{}

	driverStub := newBusyStubAgent()
	driverStub.setBusy(true)
	coord.subAgentDrivers.register("child-1", subAgentDriver{agent: driverStub})

	require.False(t, coord.asyncJobs.childScopeDrained("child-1"),
		"a busy driver must hold the child non-terminal even when c.currentAgent reports idle")

	driverStub.setBusy(false)
	require.True(t, coord.asyncJobs.childScopeDrained("child-1"),
		"once the driver is idle (and no async/background work is pending) the child is terminal")

	// A session with no registered driver falls back to c.currentAgent,
	// preserving behavior for non-delegated sessions and bare test fixtures.
	require.True(t, coord.asyncJobs.childScopeDrained("no-driver-session"),
		"a session with no registered driver must fall back to c.currentAgent's busy state")
}

// TestSubAgentDriverRegistry_ReleaseIfCurrentIsGenerationGuarded pins §6.2's
// compare-and-delete: a stale generation observed before a resume_session_id
// re-registered the same child id must never delete the newer record.
//
// Revert-check performed: replaced releaseIfCurrent's compare-and-delete
// body with an unconditional `delete(r.byChild, childSessionID); return
// true`. This test FAILED (the stale g1 release removed the g2 record,
// current.ok became false). Restored the compare-and-delete; re-ran, passed.
func TestSubAgentDriverRegistry_ReleaseIfCurrentIsGenerationGuarded(t *testing.T) {
	t.Parallel()
	r := newSubAgentDriverRegistry()
	g1 := r.register("child-1", subAgentDriver{agent: &mockSessionAgent{}})
	g2 := r.register("child-1", subAgentDriver{agent: &mockSessionAgent{}})
	require.NotEqual(t, g1, g2, "a re-registration must bump the generation")

	require.False(t, r.releaseIfCurrent("child-1", g1),
		"a stale (superseded) generation must not release the current record")
	current, ok := r.get("child-1")
	require.True(t, ok, "the newer record must still be present")
	require.Equal(t, g2, current.generation)

	require.True(t, r.releaseIfCurrent("child-1", g2),
		"the CURRENT generation must successfully release the record")
	_, ok = r.get("child-1")
	require.False(t, ok)
}

// TestReleaseDriverIfScopeClosed_RemovesDriverAndAllowlist pins §6.2's whole
// path: releasing a child's scope removes both the driver record and its
// generation-bound allowlist entry.
//
// Revert-check performed: commented out the
// `l.coord.releaseDriverIfScopeClosed(childSessionID)` call in recheckChild's
// tail (work_ledger_delegation.go) and, for this direct-unit-call variant,
// temporarily made releaseDriverIfScopeClosed a no-op. This test FAILED (the
// driver remained registered and spy.clears stayed empty). Restored both;
// re-ran, passed.
func TestReleaseDriverIfScopeClosed_RemovesDriverAndAllowlist(t *testing.T) {
	t.Parallel()
	spy := newAllowlistSpy(t)
	coord := &coordinator{permissions: spy, subAgentDrivers: newSubAgentDriverRegistry()}

	allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{Restrict: true, AllowTools: []string{"view"}})
	require.NoError(t, err)
	spy.SetSessionRunAllowlist("parent-1", allowlist)

	gen := coord.subAgentDrivers.register("child-1", subAgentDriver{agent: &mockSessionAgent{}, parentSessionID: "parent-1"})
	spy.mgr.InheritSessionRunAllowlistForGeneration("parent-1", "child-1", gen)

	coord.releaseDriverIfScopeClosed("child-1")

	_, ok := coord.subAgentDrivers.get("child-1")
	require.False(t, ok, "the driver record must be removed once the scope closes")

	spy.mu.Lock()
	defer spy.mu.Unlock()
	require.Len(t, spy.clears, 1, "the allowlist entry must be cleared exactly once")
	require.Equal(t, "child-1", spy.clears[0])
}

// TestRunSubAgent_ResumeAfterScopeClosedReArmsAllowlist pins §6.2's closing
// argument: releasing a child's scope does not strand a LATER resume without
// a policy -- resume_session_id's full runSubAgent path always re-registers
// the driver and re-inherits the allowlist before the child's next turn.
func TestRunSubAgent_ResumeAfterScopeClosedReArmsAllowlist(t *testing.T) {
	const providerID = "test-provider"
	env := testEnv(t)
	spy := newAllowlistSpy(t)
	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})
	coord.permissions = spy
	coord.subAgentDrivers = newSubAgentDriverRegistry()

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	stubAgent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		return agentResultWithText("done"), nil
	})

	_, err = coord.runSubAgent(t.Context(), subAgentParams{
		Agent: stubAgent, SessionID: parent.ID, AgentMessageID: "msg-1", ToolCallID: "call-1",
		Prompt: "do something", SessionTitle: "child",
	})
	require.NoError(t, err)

	childID := env.sessions.CreateAgentToolSessionID("msg-1", "call-1")
	_, ok := coord.subAgentDrivers.get(childID)
	require.True(t, ok, "the first delegation must register a driver")
	spy.mu.Lock()
	firstInheritCount := len(spy.inherits)
	spy.mu.Unlock()
	require.Positive(t, firstInheritCount, "the first delegation must inherit the allowlist")

	// Simulate the scope having fully closed (§6.2's release point,
	// ordinarily reached via recheckChild once the child's own work drains).
	coord.releaseDriverIfScopeClosed(childID)
	_, ok = coord.subAgentDrivers.get(childID)
	require.False(t, ok, "the driver must be gone once the scope is released")

	// Resume the SAME child session through the FULL runSubAgent path.
	_, err = coord.runSubAgent(t.Context(), subAgentParams{
		Agent: stubAgent, SessionID: parent.ID, ResumeSessionID: childID,
		Prompt: "continue",
	})
	require.NoError(t, err)

	_, ok = coord.subAgentDrivers.get(childID)
	require.True(t, ok, "resume must re-register a driver")
	spy.mu.Lock()
	defer spy.mu.Unlock()
	require.Greater(t, len(spy.inherits), firstInheritCount,
		"resume must re-inherit the allowlist BEFORE the child's turn can run again")
	last := spy.inherits[len(spy.inherits)-1]
	require.Equal(t, [2]string{parent.ID, childID}, last)
}
