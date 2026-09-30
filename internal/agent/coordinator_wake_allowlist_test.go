package agent

// §6.2/§6.4: restricted-run allowlist lifetime for a delegated child session
// must match the DRIVER's lifetime, not the first turn's -- a woken turn
// re-inherits the baseline from subAgentDriver.parentSessionID on every
// wake, and neither runSubAgent nor asyncTool.run clear the child's entry on
// return anymore.

import (
	"context"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// allowlistSpy wraps a real permission.Service and records
// Inherit/Clear calls on the SessionRunAllowlistManager extension, while
// delegating actual behavior to the real service underneath.
type allowlistSpy struct {
	permission.Service
	mgr permission.SessionRunAllowlistManager

	mu       sync.Mutex
	inherits [][2]string
	clears   []string
}

func newAllowlistSpy(t *testing.T) *allowlistSpy {
	t.Helper()
	svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
	return &allowlistSpy{Service: svc, mgr: svc.(permission.SessionRunAllowlistManager)}
}

func (s *allowlistSpy) SetSessionRunAllowlist(sessionID string, allowlist permission.RunAllowlist) {
	s.mgr.SetSessionRunAllowlist(sessionID, allowlist)
}

func (s *allowlistSpy) ClearSessionRunAllowlist(sessionID string) {
	s.mu.Lock()
	s.clears = append(s.clears, sessionID)
	s.mu.Unlock()
	s.mgr.ClearSessionRunAllowlist(sessionID)
}

func (s *allowlistSpy) InheritSessionRunAllowlist(parentID, childID string) {
	s.mu.Lock()
	s.inherits = append(s.inherits, [2]string{parentID, childID})
	s.mu.Unlock()
	s.mgr.InheritSessionRunAllowlist(parentID, childID)
}

func (s *allowlistSpy) SetSessionRunAllowlistForEpoch(sessionID string, allowlist permission.RunAllowlist, ownerEpoch uint64) {
	s.mgr.SetSessionRunAllowlistForEpoch(sessionID, allowlist, ownerEpoch)
}

func (s *allowlistSpy) ClearSessionRunAllowlistForEpoch(sessionID string, ownerEpoch uint64) {
	s.mgr.ClearSessionRunAllowlistForEpoch(sessionID, ownerEpoch)
}

func (s *allowlistSpy) SetSessionRunAllowlistForCall(sessionID string, allowlist permission.RunAllowlist, ownerCallID string) {
	s.mgr.SetSessionRunAllowlistForCall(sessionID, allowlist, ownerCallID)
}

func (s *allowlistSpy) ClearSessionRunAllowlistForCall(sessionID string, ownerCallID string) {
	s.mgr.ClearSessionRunAllowlistForCall(sessionID, ownerCallID)
}

// InheritSessionRunAllowlistForGeneration/ClearSessionRunAllowlistForGeneration
// record into the SAME inherits/clears slices as the bare (ungoverned)
// methods above: phase 3's three production call sites (runSubAgent,
// asyncTool.Run, drainCallFor) all switched to the generation-guarded pair,
// so a spy that only recorded the bare methods would never see a call again
// and every test below would silently stop observing anything.
func (s *allowlistSpy) InheritSessionRunAllowlistForGeneration(parentID, childID string, generation uint64) {
	s.mu.Lock()
	s.inherits = append(s.inherits, [2]string{parentID, childID})
	s.mu.Unlock()
	s.mgr.InheritSessionRunAllowlistForGeneration(parentID, childID, generation)
}

func (s *allowlistSpy) ClearSessionRunAllowlistForGeneration(childID string, generation uint64) {
	s.mu.Lock()
	s.clears = append(s.clears, childID)
	s.mu.Unlock()
	s.mgr.ClearSessionRunAllowlistForGeneration(childID, generation)
}

// TestWakeSession_ReArmsChildRunAllowlistBeforeWaking pins §6.2: wakeSession
// must re-inherit the driver's parentSessionID's allowlist baseline onto the
// child BEFORE waking it, even if the child's entry was cleared earlier (the
// pre-phase-2 `defer Clear` this phase removed used to do exactly that).
// Revert-check performed: removed the InheritSessionRunAllowlist call from
// drainCallFor's driver branch (agent_drain.go) -- this test FAILED (spy.inherits was
// empty). Restored the call; re-ran, passed.
func TestWakeSession_ReArmsChildRunAllowlistBeforeWaking(t *testing.T) {
	spy := newAllowlistSpy(t)
	agent := &mockSessionAgent{
		runFunc: func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("done"), nil
		},
	}
	coord := &coordinator{permissions: spy, subAgentDrivers: newSubAgentDriverRegistry()}
	coord.asyncJobs = newWorkLedger(nil)
	coord.asyncJobs.store = newTestAsyncJobStore(t)
	coord.asyncJobs.coord = coord
	coord.subAgentDrivers.register("child-1", subAgentDriver{agent: agent, parentSessionID: "parent-1"})

	// Simulate the child's allowlist entry having been cleared already
	// (what the removed `defer Clear` used to do at the end of the first turn).
	spy.ClearSessionRunAllowlist("child-1")

	require.NoError(t, coord.asyncJobs.store.InsertSessionNotice(t.Context(), "child-1", "manual_test_notice", "owed", true, ""))
	err := coord.wakeSession(t.Context(), "child-1", true)
	require.NoError(t, err)

	spy.mu.Lock()
	defer spy.mu.Unlock()
	require.NotEmpty(t, spy.inherits, "wakeSession must re-inherit the allowlist before waking a driver-owned child")
	require.Equal(t, [2]string{"parent-1", "child-1"}, spy.inherits[len(spy.inherits)-1])
}

// TestRunSubAgent_DoesNotClearChildAllowlistOnReturn pins §6.2: runSubAgent
// must NOT clear the child's allowlist entry when its own (first) turn
// returns -- the child may still own async jobs/background shells, and a
// later wake needs the entry to still be re-inheritable. Revert-check
// performed: restored the old `defer mgr.ClearSessionRunAllowlist(session.ID)`
// -- this test FAILED (spy.clears contained the child session id). Removed
// it again; re-ran, passed.
func TestRunSubAgent_DoesNotClearChildAllowlistOnReturn(t *testing.T) {
	const providerID = "test-provider"
	env := testEnv(t)
	spy := newAllowlistSpy(t)
	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})
	coord.permissions = spy
	coord.subAgentDrivers = newSubAgentDriverRegistry()

	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		return agentResultWithText("done"), nil
	})

	_, err = coord.runSubAgent(t.Context(), subAgentParams{
		Agent: agent, SessionID: parent.ID, AgentMessageID: "msg-1", ToolCallID: "call-1",
		Prompt: "do something", SessionTitle: "child",
	})
	require.NoError(t, err)

	spy.mu.Lock()
	defer spy.mu.Unlock()
	require.Empty(t, spy.clears, "runSubAgent must not clear the child's allowlist entry on return")
}

// TestAsyncTool_DoesNotClearChildAllowlistOnDelegationReturn pins §6.2 for
// the async_tool.go path (agent/agentic_fetch dispatched under asyncTool):
// t.run must not clear the delegated child's allowlist entry once its
// goroutine returns. Revert-check performed: restored the old
// `defer mgr.ClearSessionRunAllowlist(childSessionID)` inside t.run --
// this test FAILED (spy.clears contained the child session id). Removed it
// again; re-ran, passed.
func TestAsyncTool_DoesNotClearChildAllowlistOnDelegationReturn(t *testing.T) {
	spy := newAllowlistSpy(t)
	coord := &coordinator{permissions: spy}
	coord.asyncJobs = newWorkLedger(nil)
	coord.asyncJobs.store = newTestAsyncJobStore(t)
	coord.asyncJobs.coord = coord

	const owner, childSession, callID = "owner-session", "child-session", "call-1"
	_, _, err := coord.asyncJobs.Start(owner, callID, "", AgentToolName, childSession, false, false, nil, func() {})
	require.NoError(t, err)
	coord.asyncJobs.acknowledged(jobOf(coord.asyncJobs, owner, callID))

	wrapped := &asyncTool{
		inner:       newYieldedInnerTool(AgentToolName, "delegation result"),
		coordinator: coord,
		name:        AgentToolName,
	}
	ctx := WithCallOrigin(t.Context(), message.OriginWeb)
	wrapped.run(ctx, func() {}, jobOf(coord.asyncJobs, owner, callID), owner, childSession, fantasy.ToolCall{
		ID: callID, Name: AgentToolName, Input: `{}`,
	}, false)

	spy.mu.Lock()
	defer spy.mu.Unlock()
	require.Empty(t, spy.clears, "asyncTool.run must not clear the delegated child's allowlist entry on return")
}
