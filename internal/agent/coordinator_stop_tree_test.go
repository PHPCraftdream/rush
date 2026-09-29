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
	"github.com/PHPCraftdream/rush/internal/session"
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

	row, err := f.store.Get(ctx, f.sessID, "call-1")
	require.NoError(t, err)
	require.EqualValues(t, 0, row.Wake, "Stop's wake-zero pass must have closed the race")

	// A late/duplicate hint for the SAME completion (exactly what a
	// goroutine racing Stop would eventually deliver) must not produce a
	// turn: decideDrainTurn's own visible-debt check now sees wake=0.
	err = f.coord.wakeSession(ctx, jobIdentity{owner: f.sessID, toolCallID: "call-1"}, true)
	require.NoError(t, err)
	require.Zero(t, f.requests.Load(), "Stop must prevent any turn over a race-won natural completion")
}

// stopTreeChild is one child session in TestStop_NSubAgentsEachOwnBash_NoNewTurnAnywhere's
// delegation tree: its own bash job (claimed directly, in-memory too so
// l.running stays true) plus a mock driver counting provider calls.
type stopTreeChild struct {
	sessID string
	agent  *mockSessionAgent
	calls  *atomic.Int32
}

// TestStop_NSubAgentsEachOwnBash_NoNewTurnAnywhere: a root delegates to N
// child sessions, each of which has its OWN running bash job. Cancel(root)
// must walk the whole live delegation tree (workLedger.cancelTree) and zero
// every descendant's wake -- no session in the tree may get a turn
// afterward, matching doc sec.3.8's "Stop is transitive" and "a stopped
// delegation's child has no right to a turn".
//
// REVERT CHECK: changed coordinator.Cancel to call c.asyncJobs.cancelSession
// (single id) instead of cancelTree -- this test's per-child assertions
// FAILED (both children's bash jobs still had wake=1, and calling
// wakeSession for a child produced a provider call). Restored cancelTree;
// re-ran, passed.
func TestStop_NSubAgentsEachOwnBash_NoNewTurnAnywhere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const n = 2
	env := testEnv(t)
	root, err := env.sessions.Create(ctx, "stop-tree-root")
	require.NoError(t, err)

	store := newTestAsyncJobStore(t)
	coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry(), currentAgent: &mockSessionAgent{}}
	ledger := newWorkLedger(coord.notifyAsyncCompletion)
	ledger.store = store
	ledger.coord = coord
	coord.asyncJobs = ledger

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

		// Root's delegation row to this child, still RUNNING (armed) --
		// this is what makes cancelTree walk to the child at all.
		_, _, err = ledger.Start(root.ID, "delegate-"+childSess.ID, "do work", AgentToolName, childSess.ID, false, false, nil, func() {})
		require.NoError(t, err)

		// The child's OWN bash job, finished (wake=1, delivery=pending) --
		// registered in-memory too (l.Start) so its scope looks open and
		// its driver survives the release-triggered recheckChild path.
		_, existing, err := ledger.Start(childSess.ID, "call-bash", "x", "bash", "", false, false, nil, func() {})
		require.NoError(t, err)
		require.False(t, existing)
		require.NoError(t, store.MarkAnnounced(ctx, childSess.ID, "call-bash"))
		_, err = store.Transition(ctx, session.TransitionParams{
			Owner: childSess.ID, ToolCallID: "call-bash", State: "completed", ResultSummary: "done", Wake: true,
		})
		require.NoError(t, err)
	}

	coord.Cancel(root.ID)

	for _, child := range children {
		row, err := store.Get(ctx, child.sessID, "call-bash")
		require.NoError(t, err)
		require.EqualValues(t, 0, row.Wake, "child %s's own bash job must have its wake zeroed", child.sessID)

		delegationRow, err := store.Get(ctx, root.ID, "delegate-"+child.sessID)
		require.NoError(t, err)
		require.NotEqual(t, "running", delegationRow.State, "the delegation row itself must no longer be running")

		err = coord.wakeSession(ctx, jobIdentity{owner: child.sessID, toolCallID: "call-bash"}, true)
		require.NoError(t, err)
	}

	for _, child := range children {
		require.Zero(t, child.calls.Load(), "child %s must never have been driven into a new turn", child.sessID)
	}
}
