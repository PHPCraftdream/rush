// R12-E round 4 accounting funnel test: a Drain leg's batch accounting
// settles EXACTLY the snapshot captured at the leg's own turn-start pull --
// a debt row that arrives after the pull is never incremented on, never
// settled, and never named by the leg's marker. Driven through the REAL
// coordinator funnel (newCutoverLedgerCoordinator + cutoverBindFacts over a
// cutoverEnv) and the REAL batch store. COMPILE-ONLY IN THIS WORKTREE.
package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func TestBatchCutoverAccountingFunnelON(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	fast := &countingTool{name: "view"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, fast)
	coord, _ := newCutoverLedgerCoordinator(t)
	cutoverBindFacts(t, coord, env)
	facts := coordinatorBatchCutoverFacts(coord)

	// A: settled AND pulled (visible debt) -- the acted-on set the leg's
	// pull captured.
	env.invokeDetached("call-funnel-a", "view", fast)
	rootA := env.rootID("call-funnel-a")
	require.Equal(t, "completed", env.rootTerminal(rootA))
	env.requireBatchPull(1)
	snapA, err := facts.CaptureVisibleDebt(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.Len(t, snapA.Nodes, 1, "A is the one visible debt row")
	require.Equal(t, rootA, snapA.Nodes[0].RootID)

	// B: a SECOND settled+delivered root whose debt "arrives after the
	// pull" -- it must NOT be in the acted-on set.
	env.invokeDetached("call-funnel-b", "view", fast)
	rootB := env.rootID("call-funnel-b")
	require.Equal(t, "completed", env.rootTerminal(rootB))
	env.requireBatchPull(1)
	both, err := facts.CaptureVisibleDebt(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.Len(t, both.Nodes, 2, "both roots are visible debt before the accounting")

	// Account the leg with the RETAINED post-pull snapshot and a terminal
	// provider failure: decideAccount yields VClose over att.batchSnapshot.
	att := &drainAttempt{
		sessionID: env.sessionID, outcome: drainAttempted,
		batchSnapshot: snapA, batchCaptured: true,
	}
	terminal := &fantasy.ProviderError{StatusCode: 402, Message: "quota exhausted"}
	coord.accountDrainAttempt(context.Background(), att, terminal)

	// The acted-on root's rows were closed; the late root's row SURVIVED.
	after, err := facts.CaptureVisibleDebt(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.Len(t, after.Nodes, 1, "only the post-pull row remains")
	require.Equal(t, rootB, after.Nodes[0].RootID,
		"the root that arrived after the pull keeps its open debt row")
	visible, err := env.store.BatchReactionVisibleDebtExists(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.True(t, visible, "B's debt row survived the leg's settle")
	// The settle marker names ONLY the acted-on root.
	require.Equal(t, []string{"call-funnel-a"}, facts.DebtToolCallIDs(env.ctx, snapA),
		"the leg's marker never names the late root")
	env.logDebtRows(t)
	env.logNodeRows(t, rootA, rootB)
}
