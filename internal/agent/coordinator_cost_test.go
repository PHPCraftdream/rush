package agent

// Subtree-budget tests for sub-agent spend (#1130): the parent's budget
// accumulates every delegation child's own ledger, nothing is transferred,
// and attribution stays per-node (the parent's own row carries only its own
// spend). These replace the old updateParentSessionCost delta-ledger tests.

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func budgetOf(t *testing.T, env fakeEnv, sessionID string) float64 {
	t.Helper()
	budget, err := env.sessions.SubtreeBudget(context.Background(), sessionID)
	require.NoError(t, err)
	return budget
}

func TestSubtreeBudget(t *testing.T) {
	t.Run("accumulates child costs", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}
		_ = coord

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		child1, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child1")
		require.NoError(t, err)
		_, err = env.sessions.IncrementCost(t.Context(), child1.ID, 0.05)
		require.NoError(t, err)

		child2, err := env.sessions.CreateTaskSession(t.Context(), "tool-2", parent.ID, "Child2")
		require.NoError(t, err)
		_, err = env.sessions.IncrementCost(t.Context(), child2.ID, 0.03)
		require.NoError(t, err)

		assert.InDelta(t, 0.08, budgetOf(t, env, parent.ID), 1e-9,
			"the parent's budget holds both children's spend, exactly once")
		assert.InDelta(t, 0.05, budgetOf(t, env, child1.ID), 1e-9,
			"the subtree includes the node itself: a child without its own delegations reads exactly its own ledger")

		// Attribution: the parent's OWN ledger carries nothing; a read that
		// wants per-node numbers gets per-node numbers.
		parent, err = env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.0, parent.OwnCost, 1e-9)
	})

	t.Run("budget unknown session errors", func(t *testing.T) {
		env := testEnv(t)
		_, err := env.sessions.SubtreeBudget(context.Background(), "non-existent")
		require.Error(t, err)
	})

	t.Run("zero cost subtree", func(t *testing.T) {
		env := testEnv(t)
		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		_, err = env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		assert.InDelta(t, 0.0, budgetOf(t, env, parent.ID), 1e-9)
	})

	// Attribution of a repeated charge stays exactly once: two increments on
	// the child (0.07 then 0.02) are both the child's own ledger entries;
	// the budget reads the sum, never the sum twice.
	t.Run("charges accumulate exactly once", func(t *testing.T) {
		env := testEnv(t)
		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		_, err = env.sessions.IncrementCost(t.Context(), child.ID, 0.07)
		require.NoError(t, err)
		assert.InDelta(t, 0.07, budgetOf(t, env, parent.ID), 1e-9)

		_, err = env.sessions.IncrementCost(t.Context(), child.ID, 0.02)
		require.NoError(t, err)
		assert.InDelta(t, 0.09, budgetOf(t, env, parent.ID), 1e-9,
			"the budget reflects the child's total exactly once, not double-counted")

		// The reset freezes the budget at the current spend; new child spend
		// shows up as budget growth on top of the base.
		require.NoError(t, env.sessions.ResetCostBase(t.Context(), parent.ID))
		assert.InDelta(t, 0.0, budgetOf(t, env, parent.ID), 1e-9)
		_, err = env.sessions.IncrementCost(t.Context(), child.ID, 0.01)
		require.NoError(t, err)
		assert.InDelta(t, 0.01, budgetOf(t, env, parent.ID), 1e-9)
	})
}

// TestRunSubAgentCostSurvivesCancelledParentContext keeps the 2026-07-30
// incident's regression alive in the new model: a sub-agent that finishes
// AFTER its parent's ctx was cancelled must still have its spend visible to
// the parent's budget. The child charges its own cost_self during Run (the
// charge rides the agent's own write path, not the parent's ctx), so there
// is no post-Run transfer left to be killed by a dead context — the budget
// read at any later moment sees it.
func TestRunSubAgentCostSurvivesCancelledParentContext(t *testing.T) {
	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{ID: providerID}

	env := testEnv(t)
	coord := newTestCoordinator(t, env, providerID, providerCfg)

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())

	agent := newMockAgent(providerID, 4096, func(runCtx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		// Child incurs cost while ctx is still live...
		if _, err := env.sessions.IncrementCost(runCtx, call.SessionID, 0.09); err != nil {
			return nil, err
		}
		// ...then the parent's watchdog fires, cancelling ctx right before
		// this call returns.
		cancel()
		return agentResultWithText("done"), nil
	})

	resp, err := coord.runSubAgent(ctx, subAgentParams{
		Agent:          agent,
		SessionID:      parentSession.ID,
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Prompt:         "test",
		SessionTitle:   "Test",
	})
	require.NoError(t, err, "runSubAgent itself must not fail just because the parent ctx was cancelled")
	require.False(t, resp.IsError, "sub-agent response must not be an error just because the parent ctx was cancelled: %v", resp)

	assert.InDelta(t, 0.09, budgetOf(t, env, parentSession.ID), 1e-9,
		"the child's cost must be visible to the parent's budget despite the cancelled parent ctx")
}
