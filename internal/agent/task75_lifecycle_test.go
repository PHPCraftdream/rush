package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestTask75BatchLifecycleBindsBeforeProviderAndKeepsChildSelected(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	env := testEnv(t)
	const providerID = "task75-provider"
	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})
	coord.subAgentDrivers = newSubAgentDriverRegistry()
	parent, err := env.sessions.Create(t.Context(), "task75 parent")
	require.NoError(t, err)

	var events []string
	var noChild atomic.Int64
	lifecycle := batchAgentLifecycle{
		BindChildSession: func(_ context.Context, id string) error {
			events = append(events, "bind:"+id)
			return nil
		},
		NoChildSelected: func() { noChild.Add(1) },
	}
	childAgent := newMockAgent(providerID, 4096, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		events = append(events, "provider:"+call.SessionID)
		return agentResultWithText("done"), nil
	})

	_, err = coord.runSubAgent(withBatchAgentLifecycle(t.Context(), lifecycle), subAgentParams{
		Agent: childAgent, SessionID: parent.ID, AgentMessageID: "task75-message", ToolCallID: "task75-call",
		Prompt: "run", SessionTitle: "task75 child",
	})
	require.NoError(t, err)
	require.Len(t, events, 2)
	childID := env.sessions.CreateAgentToolSessionID("task75-message", "task75-call")
	require.Equal(t, []string{"bind:" + childID, "provider:" + childID}, events)
	require.Zero(t, noChild.Load(), "successful child selection must not be reported as absent")
}

func TestTask75ResumeLifecycleCleanupFollowsValidBind(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	env := testEnv(t)
	const providerID = "task75-provider"
	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})
	coord.subAgentDrivers = newSubAgentDriverRegistry()
	parent, err := env.sessions.Create(t.Context(), "task75 resume parent")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(t.Context(), env.sessions.CreateAgentToolSessionID("task75-existing", "task75-child"), parent.ID, "task75 resumed child")
	require.NoError(t, err)
	require.NoError(t, env.sessions.RequestCancel(t.Context(), child.ID))
	coord.claimAutoResumeSlot(child.ID, 0, true)
	coord.suspendAutoResume(child.ID)

	var noChild atomic.Int64
	lifecycle := batchAgentLifecycle{
		BindChildSession: func(_ context.Context, id string) error {
			require.Equal(t, child.ID, id)
			require.Equal(t, 1, coord.consecutiveResume(id), "resume state must remain until the child is successfully bound")
			require.True(t, coord.autoResumeSuspended(id), "suspension must remain until after bind")
			return nil
		},
		NoChildSelected: func() { noChild.Add(1) },
	}
	var providerCalled atomic.Bool
	childAgent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		providerCalled.Store(true)
		if call.SessionID != child.ID {
			return nil, errors.New("provider received unexpected child session")
		}
		requested, requestErr := env.sessions.IsCancelRequested(ctx, child.ID)
		if requestErr != nil {
			return nil, requestErr
		}
		if requested || coord.consecutiveResume(child.ID) != 0 || coord.autoResumeSuspended(child.ID) {
			return nil, errors.New("resume lifecycle state was not cleared after bind")
		}
		return agentResultWithText("resumed"), nil
	})

	_, err = coord.runSubAgent(withBatchAgentLifecycle(t.Context(), lifecycle), subAgentParams{
		Agent: childAgent, SessionID: parent.ID, ResumeSessionID: child.ID, Prompt: "continue",
	})
	require.NoError(t, err)
	require.True(t, providerCalled.Load(), "valid resume must run the child after lifecycle binding")
	require.Zero(t, noChild.Load())
}

func TestTask75FailedResumeBindPreservesLifecycleState(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	env := testEnv(t)
	const providerID = "task75-provider"
	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})
	coord.subAgentDrivers = newSubAgentDriverRegistry()
	parent, err := env.sessions.Create(t.Context(), "task75 failed resume parent")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(t.Context(), env.sessions.CreateAgentToolSessionID("task75-failed", "task75-child"), parent.ID, "task75 failed child")
	require.NoError(t, err)
	require.NoError(t, env.sessions.RequestCancel(t.Context(), child.ID))
	coord.claimAutoResumeSlot(child.ID, 0, true)
	coord.suspendAutoResume(child.ID)

	injected := errors.New("task75 lifecycle bind failed")
	var noChild atomic.Int64
	lifecycle := batchAgentLifecycle{
		BindChildSession: func(_ context.Context, id string) error {
			require.Equal(t, child.ID, id)
			return injected
		},
		NoChildSelected: func() { noChild.Add(1) },
	}
	var providerCalled atomic.Bool
	childAgent := newMockAgent(providerID, 4096, func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
		providerCalled.Store(true)
		return agentResultWithText("must not run"), nil
	})

	_, err = coord.runSubAgent(withBatchAgentLifecycle(t.Context(), lifecycle), subAgentParams{
		Agent: childAgent, SessionID: parent.ID, ResumeSessionID: child.ID, Prompt: "continue",
	})
	require.ErrorIs(t, err, injected, "the lifecycle bind wrapper must preserve its cause")
	require.False(t, providerCalled.Load(), "failed bind must stop before child provider execution")
	require.Equal(t, 1, coord.consecutiveResume(child.ID), "failed bind must not reset resume count")
	require.True(t, coord.autoResumeSuspended(child.ID), "failed bind must not lift suspension")
	requested, err := env.sessions.IsCancelRequested(t.Context(), child.ID)
	require.NoError(t, err)
	require.True(t, requested, "failed bind must leave the stale cancel request untouched")
	require.Equal(t, int64(1), noChild.Load(), "failed bind reports no selected child exactly once")
}
