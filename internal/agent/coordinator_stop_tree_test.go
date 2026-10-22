// Stop-tree coverage (docs/plans/2026-09-28-async-phase4-durable-core.md
// sec.3.4/3.8, DUR-9, step 4 review): a race between a natural completion
// and Stop must never grant a turn, for the stopped session itself or any
// descendant in its delegation tree. Real SQLite throughout.
package agent

import (
	"context"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// TestStop_OneSecondAfterNaturalFinish_NoNewTurn: an async job's natural
// finish already committed wake=1/delivery=pending (as if a moment before
// Stop). Cancel's wake-zero pass must close that race: a late hint arriving
// for the same row afterward must not produce a provider turn.
//
// REVERT CHECK (re-verified live, W-DRAIN B1, docs/reviews/2026-09-29-
// async-phase4-round1.md flagged this comment as possibly stale): gated the
// `store.SetWakeZeroForOwners` call inside coordinator.Cancel
// (coordinator_interrupt.go) behind `if false &&` -- this test's
// `require.Zero(t, f.requests.Load())` FAILED (the probe server received a
// request: the late hint still found wake=1 and ran an empty-prompt turn
// over the notice), while the SIBLING test in this package,
// TestSessionDrainPolicy_StopSuspendsUntilHumanMessage (coordinator_wake_
// reaction_debt_test.go), stayed GREEN under the same change -- proving this
// test pins the wake-zero pass specifically, independently of
// suspendAutoResume (that test's own revert-check is the mirror: it fails
// when suspendAutoResume is disabled while this mechanism is untouched, and
// stays green when the wake-zero pass alone is disabled). Restored the
// call; re-ran, both passed. Two independent mechanisms, two independent
// tests, verified NOT stale.
func TestStop_OneSecondAfterNaturalFinish_NoNewTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newWakeDebtFixture(t, "stop-after-finish")
	f.claimAndFinish(t, ctx, "call-1") // wake=1, delivery=pending, as a natural finish leaves it

	f.coord.Cancel(f.sessID) // "Stop one second after" -- the row already committed

	// Dual-mode readback: under the flag the seeded debt lives in
	// batch_nodes (claimAndFinish's ON branch), so the legacy async_jobs
	// Get has no row to return ("sql: no rows"). readFixtureJobDebt maps
	// the selected child node's wake onto the same AsyncJob view.
	row := readFixtureJobDebt(t, f.env.conn, f.sessID, "call-1")
	require.EqualValues(t, 0, row.Wake, "Stop's wake-zero pass must have closed the race")

	// A late/duplicate hint for the SAME completion (exactly what a
	// goroutine racing Stop would eventually deliver) must not produce a
	// turn: decideDrainTurn's own visible-debt check now sees wake=0.
	err := f.coord.wakeSession(ctx, f.sessID, true)
	require.NoError(t, err)
	require.Zero(t, f.requests.Load(), "Stop must prevent any turn over a race-won natural completion")
}

// stopTreeChild is one child session in
// TestStop_NSubAgentsEachOwnBash_NoNewTurnAnywhere's delegation tree: its own
// finished wake-armed bash debt plus a mock driver counting provider calls.
type stopTreeChild struct {
	sessID string
	agent  *mockSessionAgent
	calls  *atomic.Int32
}

// TestStop_NSubAgentsEachOwnBash_NoNewTurnAnywhere: a root delegates to N
// child sessions, each of which has its OWN finished, wake-armed bash debt in
// batch_nodes. Cancel(root) must walk the whole live delegation tree from the
// batch facts (stopTree's LiveDelegationNodes walk) and zero every
// descendant's wake -- no session in the tree may get a turn afterward,
// matching doc sec.3.8's "Stop is transitive" and "a stopped delegation's
// child has no right to a turn". (The batch twin over the full runtime,
// including the delegation roots' terminal projection by the batch runner,
// is coordinator_batch_stop_test.go.)
//
// REVERT CHECK (ledger era): changed coordinator.Cancel to walk a single id
// instead of the tree -- this test's per-child assertions FAILED (both
// children's bash debts still had wake=1, and calling wakeSession for a child
// produced a provider call). The same mutation in the batch world (dropping
// the LiveDelegationNodes walk from stopTree) leaves the seeded children out
// of the wake-zero owner set and this test red the same way.
func TestStop_NSubAgentsEachOwnBash_NoNewTurnAnywhere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n = 2
	env := testEnv(t)
	root, err := env.sessions.Create(ctx, "stop-tree-root")
	require.NoError(t, err)

	coord := a2AttachBatchFacts(t, env)
	coord.currentAgent = &mockSessionAgent{}

	children := make([]stopTreeChild, n)
	for i := range n {
		childSess, err := env.sessions.Create(ctx, "stop-tree-child")
		require.NoError(t, err)
		calls := &atomic.Int32{}
		children[i] = stopTreeChild{sessID: childSess.ID, calls: calls, agent: &mockSessionAgent{
			runFunc: func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
				calls.Add(1)
				return agentResultWithText("should never run"), nil
			},
		}}
		coord.subAgentDrivers.register(childSess.ID, subAgentDriver{agent: children[i].agent, call: SessionAgentCall{SessionID: childSess.ID}})

		// Root's live delegation link to this child -- this is what makes
		// stopTree's batch walk reach the child at all.
		seedBatchDelegation(t, env.conn, root.ID, "delegate-"+childSess.ID, childSess.ID, "agent", "running", false)

		// The child's OWN bash debt, finished with wake=1 and delivery
		// still pending (a natural finish a moment before Stop).
		seedBatchJobDebt(t, env.conn, childSess.ID, "call-bash", false)
	}

	coord.Cancel(root.ID)

	for _, child := range children {
		row := readFixtureJobDebt(t, env.conn, child.sessID, "call-bash")
		require.EqualValues(t, 0, row.Wake, "child %s's own bash debt must have its wake zeroed", child.sessID)

		require.True(t, coord.arb.snapshot(child.sessID).Suspended,
			"the transitive stop must suspend every tree member, %s included", child.sessID)

		err := coord.wakeSession(ctx, child.sessID, true)
		require.NoError(t, err)
	}

	for _, child := range children {
		require.Zero(t, child.calls.Load(), "child %s must never have been driven into a new turn", child.sessID)
	}
}
