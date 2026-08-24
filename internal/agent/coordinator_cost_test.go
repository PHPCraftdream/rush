package agent

// Cost-transfer tests for sub-agent spend: updateParentSessionCost's delta
// ledger (idempotency, double-charge prevention) and the regression proving
// a child's cost survives a cancelled parent context.

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpdateParentSessionCost(t *testing.T) {
	t.Run("accumulates cost correctly", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		// Set child cost via the additive API (Save no longer writes cost).
		_, err = env.sessions.IncrementCost(t.Context(), child.ID, 0.10)
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, parent.ID)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.10, updated.Cost, 1e-9)
	})

	t.Run("accumulates multiple child costs", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

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

		err = coord.updateParentSessionCost(t.Context(), child1.ID, parent.ID)
		require.NoError(t, err)
		err = coord.updateParentSessionCost(t.Context(), child2.ID, parent.ID)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.08, updated.Cost, 1e-9)
	})

	t.Run("child session not found", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), "non-existent", parent.ID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "get child session")
	})

	t.Run("parent session not found", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, "non-existent")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "increment parent session cost")
	})

	t.Run("zero cost handled correctly", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		err = coord.updateParentSessionCost(t.Context(), child.ID, parent.ID)
		require.NoError(t, err)

		updated, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.0, updated.Cost, 1e-9)
	})

	// Required test (в): idempotency via the persisted parent_cost_accounted
	// ledger. A repeat call with no new child cost accrued between them must
	// charge the parent exactly zero — the second call reads accounted == cost
	// and so computes delta 0.
	t.Run("idempotent repeat call charges zero with no new child cost", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		_, err = env.sessions.IncrementCost(t.Context(), child.ID, 0.07)
		require.NoError(t, err)

		// First transfer charges the full 0.07 delta (accounted was 0).
		require.NoError(t, coord.updateParentSessionCost(t.Context(), child.ID, parent.ID))
		afterFirst, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.07, afterFirst.Cost, 1e-9)

		// Second transfer, immediately, with no new child cost: delta is now
		// cost(0.07) - accounted(0.07) == 0, so the parent must NOT be charged.
		require.NoError(t, coord.updateParentSessionCost(t.Context(), child.ID, parent.ID))
		afterSecond, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.07, afterSecond.Cost, 1e-9,
			"a repeat call with no new child cost must charge zero (idempotency via parent_cost_accounted)")
	})

	// Direct-unit-test counterpart to the runSubAgent-level "cost accounting
	// is not skipped on the resume path" test: proves the persisted-ledger
	// delta logic at the updateParentSessionCost layer itself. The baseline no
	// longer comes from an in-memory parameter — it is read from the child's
	// parent_cost_accounted column inside the transfer transaction, so only
	// cost accrued since the last successful transfer is charged.
	t.Run("charges only the delta since the last transfer, not the raw total", func(t *testing.T) {
		env := testEnv(t)
		cfg, err := config.Init(env.workingDir, "", false)
		require.NoError(t, err)
		coord := &coordinator{cfg: cfg, sessions: env.sessions}

		parent, err := env.sessions.Create(t.Context(), "Parent")
		require.NoError(t, err)
		child, err := env.sessions.CreateTaskSession(t.Context(), "tool-1", parent.ID, "Child")
		require.NoError(t, err)

		// Child accrues 0.03 (e.g. before an ask_question pause); first
		// transfer charges the full 0.03 delta (accounted was 0).
		_, err = env.sessions.IncrementCost(t.Context(), child.ID, 0.03)
		require.NoError(t, err)
		require.NoError(t, coord.updateParentSessionCost(t.Context(), child.ID, parent.ID))

		afterPause, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.03, afterPause.Cost, 1e-9)

		// Child resumes and accrues another 0.02 (total now 0.05). The next
		// transfer reads accounted=0.03 and charges only delta 0.02, not the
		// full 0.05 again.
		_, err = env.sessions.IncrementCost(t.Context(), child.ID, 0.02)
		require.NoError(t, err)
		require.NoError(t, coord.updateParentSessionCost(t.Context(), child.ID, parent.ID))

		afterResume, err := env.sessions.Get(t.Context(), parent.ID)
		require.NoError(t, err)
		assert.InDelta(t, 0.05, afterResume.Cost, 1e-9,
			"parent must reflect the child's total exactly once (0.05), not double-charge the pre-pause portion (0.08)")
	})
}

// TestRunSubAgentCostSurvivesCancelledParentContext is the regression test
// for the 2026-07-30 incident ("Failed to update parent session cost"
// error="begin transaction: context deadline exceeded"): a sub-agent that
// finishes AFTER its parent's ctx was already cancelled (stream watchdog
// timeout, or a user Ctrl-C) must still have its spend transferred to the
// parent. Before the fix, runSubAgent called updateParentSessionCost with
// the same (now-cancelled) ctx used for the whole turn, so BeginTx failed
// immediately with context.Canceled, the transfer was skipped, and — since
// this child is never resumed again — that cost was gone for good.
func TestRunSubAgentCostSurvivesCancelledParentContext(t *testing.T) {
	const providerID = "test-provider"
	providerCfg := config.ProviderConfig{ID: providerID}

	env := testEnv(t)
	coord := newTestCoordinator(t, env, providerID, providerCfg)

	parentSession, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	// ctx is cancelled from WITHIN the mock agent's Run, i.e. after session
	// creation (which needs a live ctx and is not what we're testing) has
	// already completed, but before runSubAgent gets to the cost-transfer
	// step that runs right after Run() returns. This mirrors the real
	// incident: the parent's stream watchdog (or a user Ctrl-C) fires while
	// the child sub-agent call is in flight, and the child still manages to
	// persist its cost and return a result before the cancellation is
	// observed anywhere else — but the cost-transfer that runs immediately
	// after must not be handed the now-dead ctx.
	ctx, cancel := context.WithCancel(t.Context())

	agent := newMockAgent(providerID, 4096, func(runCtx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		// Child incurs cost while ctx is still live...
		if _, err := env.sessions.IncrementCost(runCtx, call.SessionID, 0.09); err != nil {
			return nil, err
		}
		// ...then the parent's watchdog fires, cancelling ctx right before
		// this call returns — simulating cancellation racing the child's
		// completion.
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

	updatedParent, err := env.sessions.Get(t.Context(), parentSession.ID)
	require.NoError(t, err)
	assert.InDelta(t, 0.09, updatedParent.Cost, 1e-9,
		"the child's cost must still be transferred to the parent despite the cancelled parent ctx")
}
