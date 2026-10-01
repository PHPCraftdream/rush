// #1130: the delegation cost tests, rewritten for the subtree-read model.
// The transfer ledger is gone: a child charges its OWN cost_self as it runs,
// and the parent's "including children" number is Service.SubtreeBudget —
// a query over the delegation tree at read time. The old R6C-3 hazard
// (a Drain turn's spend reaching the parent only via a release-ordered
// transfer) is structurally gone: there is no moment the spend is not
// already visible to the query.
package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// spendingAgent is a driver whose every Run leaves delta on the child's cost
// (a provider turn that cost money) before it returns.
type spendingAgent struct {
	SessionAgent
	f     *attemptFixture
	child string
	delta float64
}

func (a *spendingAgent) Run(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	res, err := a.SessionAgent.Run(ctx, call)
	if _, incErr := a.f.env.sessions.IncrementCost(context.WithoutCancel(ctx), a.child, a.delta); incErr != nil {
		return res, incErr
	}
	return res, err
}

func (f *attemptFixture) sessionCost(id string) float64 {
	f.t.Helper()
	sess, err := f.env.sessions.Get(context.Background(), id)
	require.NoError(f.t, err)
	return sess.OwnCost
}

func (f *attemptFixture) subtreeBudget(id string) float64 {
	f.t.Helper()
	budget, err := f.env.sessions.SubtreeBudget(context.Background(), id)
	require.NoError(f.t, err)
	return budget
}

// TestDelegationRelease_ChildSpendVisibleToTheParentBudget: the child's
// first turn costs $0.10, its Drain turn $0.30, the delegation then
// releases. The parent's BUDGET holds both at every read, with no
// transfer ordering to get wrong, and a release moves nothing anywhere.
//
// Revert-check: dropping the delegation flag from CreateTaskSession (the
// cost-tree edge) makes SubtreeBudget stop seeing the child entirely and
// this test goes red at the first assertion.
func TestDelegationRelease_ChildSpendVisibleToTheParentBudget(t *testing.T) {
	ctx := context.Background()
	c := newChildBGFixture(t, childBGMode{noIdle: true})
	c.runs.SessionAgent = &spendingAgent{SessionAgent: c.sa, f: c.attemptFixture, child: c.childID, delta: 0.30}

	// The first turn: $0.10 lands in the child's own ledger and is already
	// visible to the parent's budget before anything releases.
	_, err := c.env.sessions.IncrementCost(ctx, c.childID, 0.10)
	require.NoError(t, err)
	require.InDelta(t, 0.10, c.subtreeBudget(c.parentID), 1e-9)
	require.InDelta(t, 0, c.sessionCost(c.parentID), 1e-9, "the parent's own ledger carries nothing")
	c.park()

	c.finishShell() // the Drain turn: +$0.30 on the child
	got := c.awaitRelease()
	require.Equal(t, "delegate-1", got.ToolCallID)

	require.InDelta(t, 0.40, c.subtreeBudget(c.parentID), 1e-9, "both turns, exactly once")
	require.InDelta(t, 0, c.sessionCost(c.parentID), 1e-9, "no transfer ever touched the parent's row")
	require.InDelta(t, 0.40, c.sessionCost(c.childID), 1e-9, "the child's ledger keeps the full spend")

	// A second release moves nothing anywhere (there is nothing to move).
	c.coord.afterRelease(c.childID)
	require.InDelta(t, 0.40, c.subtreeBudget(c.parentID), 1e-9)
	require.InDelta(t, 0, c.sessionCost(c.parentID), 1e-9)
}

// TestChildRunEnd_ReleaseChargesNothing: a real Stop of a parked delegation
// after the child's Drain turn spent $0.30. The release machinery used to be
// the only path the Drain spend had to the parent; now the budget was
// complete all along, and the release is pure lifecycle: the driver is
// released, no cost moves, and a root's release is a no-op.
//
// Revert-check: re-adding any parent charge in afterRelease makes the
// parent's own ledger non-zero and this test red.
func TestChildRunEnd_ReleaseChargesNothing(t *testing.T) {
	ctx := context.Background()
	c := newChildBGFixture(t, childBGMode{noIdle: true})
	c.coord.currentAgent = &mockSessionAgent{} // a root has no driver: Stop and the release route to it
	c.park()
	_, err := c.env.sessions.IncrementCost(ctx, c.childID, 0.10)
	require.NoError(t, err)

	// A Drain turn spends $0.30, then the operator stops the delegation.
	_, err = c.env.sessions.IncrementCost(ctx, c.childID, 0.30)
	require.NoError(t, err)
	c.coord.Cancel(c.parentID)
	require.False(t, c.ledger.hasParked(), "the stopped delegation is terminal")
	_, registered := c.coord.subAgentDrivers.get(c.childID)
	require.True(t, registered, "the child's shell still holds its scope: the driver stays until the run end")
	require.InDelta(t, 0.40, c.subtreeBudget(c.parentID), 1e-9, "the budget never waited for a transfer")

	// The shell ends and the child's mailbox is released: lifecycle only.
	require.NoError(t, c.mgr.Kill(ctx, c.held.ID))
	c.coord.afterRelease(c.childID)

	_, registered = c.coord.subAgentDrivers.get(c.childID)
	require.False(t, registered, "and its driver is released")
	require.InDelta(t, 0.40, c.subtreeBudget(c.parentID), 1e-9)
	require.InDelta(t, 0, c.sessionCost(c.parentID), 1e-9, "the release moved no cost")
	c.coord.afterRelease(c.childID) // a second release still moves nothing
	require.InDelta(t, 0.40, c.subtreeBudget(c.parentID), 1e-9)

	// A root session's release charges nobody.
	c.coord.afterRelease(c.parentID)
	require.InDelta(t, 0.40, c.subtreeBudget(c.parentID), 1e-9)
	require.InDelta(t, 0, c.sessionCost(c.sessID), 1e-9)
}
