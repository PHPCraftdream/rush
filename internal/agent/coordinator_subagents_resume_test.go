package agent

// #1036: runSubAgent must not report "Sub-agent completed but produced no
// text output" for a child whose mailbox was merely busy when this call
// tried it -- see runAwaitingAdmission (coordinator_run_await.go).

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// busyThenResolvingAgent simulates a SessionAgent whose mailbox is busy on
// the first Run attempt: it marks the ctx's turnAdmission queued (exactly
// what the real sessionAgent.Run does via tryReserveSession) and stashes the
// call so the test can fire its onQueueResolved later, standing in for
// runOwned's own dispatch loop draining a queued call.
type busyThenResolvingAgent struct {
	mockSessionAgent
	mu      sync.Mutex
	busy    bool
	pending SessionAgentCall
}

func (a *busyThenResolvingAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	a.mu.Lock()
	busy := a.busy
	if busy {
		a.pending = call
	}
	a.mu.Unlock()
	if !busy {
		return agentResultWithText("direct result"), nil
	}
	if adm := agentcall.TurnAdmissionFrom(ctx); adm != nil {
		adm.MarkQueued()
	}
	return nil, nil
}

func (a *busyThenResolvingAgent) resolvePending(result *fantasy.AgentResult, err error) {
	a.mu.Lock()
	call := a.pending
	a.mu.Unlock()
	if call.OnQueueResolved != nil {
		call.OnQueueResolved(result, err)
	}
}

func (a *busyThenResolvingAgent) hasPending() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pending.OnQueueResolved != nil
}

// TestRunSubAgent_ResumeOfBusyChildDoesNotReportNoOutput pins #1036: if the
// child's mailbox is busy when runSubAgent's own Run attempt lands (e.g.
// notifyAsyncCompletion just woke it for the child's OWN async-job
// completion a moment earlier), the parent must get the REAL result once
// the queued turn resolves, not "no text output" about a turn that never
// started.
// Revert-check performed: reverted `run` in coordinator_subagents.go to call
// params.Agent.Run(ctx, ...) directly (bypassing runAwaitingAdmission) --
// this test FAILED (response was "Sub-agent completed but produced no text
// output."). Restored the runAwaitingAdmission call; re-ran, passed.
func TestRunSubAgent_ResumeOfBusyChildDoesNotReportNoOutput(t *testing.T) {
	const providerID = "test-provider"
	env := testEnv(t)
	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{
		ID:     providerID,
		Models: []catwalk.Model{{ID: "test-model", DefaultMaxTokens: 4096}},
	})
	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	agent := &busyThenResolvingAgent{busy: true}
	agent.model = Model{
		CatwalkCfg: catwalk.Model{DefaultMaxTokens: 4096},
		ModelCfg:   config.SelectedModel{Provider: providerID, Model: "test-model"},
	}

	type outcome struct {
		resp fantasy.ToolResponse
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		resp, runErr := coord.runSubAgent(t.Context(), subAgentParams{
			Agent: agent, SessionID: parent.ID, AgentMessageID: "msg-1", ToolCallID: "call-1",
			Prompt: "continue", SessionTitle: "child",
		})
		done <- outcome{resp, runErr}
	}()

	require.Eventually(t, agent.hasPending, 2*time.Second, 10*time.Millisecond,
		"runSubAgent's Run attempt must have queued behind the busy child")

	agent.resolvePending(agentResultWithText("real sub-agent output"), nil)

	select {
	case o := <-done:
		require.NoError(t, o.err)
		require.Equal(t, "real sub-agent output", o.resp.Content)
		require.NotContains(t, o.resp.Content, "no text output")
	case <-time.After(2 * time.Second):
		t.Fatal("runSubAgent did not return after the queued turn resolved")
	}
}

func TestRunSubAgent_ResumeOfMissingChildListsRecentChildren(t *testing.T) {
	const providerID = "test-provider"
	env := testEnv(t)
	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{
		ID:     providerID,
		Models: []catwalk.Model{{ID: "test-model", DefaultMaxTokens: 4096}},
	})
	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	children := make([]string, 6)
	for i := range children {
		child, createErr := env.sessions.CreateTaskSession(t.Context(), fmt.Sprintf("child-%d", i), parent.ID, fmt.Sprintf("child %d", i))
		require.NoError(t, createErr)
		children[i] = child.ID
	}
	agent := newMockAgent(providerID, 4096, func(_ context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		return agentResultWithText("should not happen"), nil
	})
	resp, err := coord.runSubAgent(t.Context(), subAgentParams{
		Agent: agent, SessionID: parent.ID, AgentMessageID: "msg-missing", ToolCallID: "call-missing",
		Prompt: "answer", SessionTitle: "child", ResumeSessionID: "does-not-exist",
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "known child sessions of this session:")
	newest, secondNewest, oldest := children[5], children[4], children[0]
	require.Less(t, strings.Index(resp.Content, newest), strings.Index(resp.Content, secondNewest))
	require.NotContains(t, resp.Content, oldest)
	for _, id := range children[1:] {
		require.Contains(t, resp.Content, id)
	}
}
