// R6C-3: a delegated child's spend after its first turn (its Drain turns) must
// reach the parent's cost whichever way the delegation ends. runSubAgent
// charges the parent when the child's FIRST turn returns; the child's reaction
// turns run on its driver later and were charged to the child alone, so a
// normal exit (no running row left for the app's chargeRunningChildren)
// reported less than a Ctrl-C exit for the same work. The transfer is the
// session store's parent_cost_accounted delta, so every charge below is
// idempotent with the others.
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
	return sess.Cost
}

// TestDelegationRelease_ChargesChildReactionSpendBeforeTheNoticeCommits: the
// child's first turn cost $0.10 and was transferred; its Drain turn costs $0.30;
// the delegation then releases. The parent's cost holds both, exactly once,
// and it already held them when the release's notice committed (a SQLite
// trigger records the parent's cost at that commit): a `rush run` loop that
// sees the notice as debt and exits reads the full cost. A late charge (the
// child's own return, Shutdown, chargeRunningChildren) adds nothing.
//
// Revert-check: dropping the transfer from recheckChild's release leaves the
// parent at $0.10 at the commit and after it, and this test goes red.
func TestDelegationRelease_ChargesChildReactionSpendBeforeTheNoticeCommits(t *testing.T) {
	ctx := context.Background()
	c := newChildBGFixture(t, childBGMode{noIdle: true})
	c.runs.SessionAgent = &spendingAgent{SessionAgent: c.sa, f: c.attemptFixture, child: c.childID, delta: 0.30}
	c.exec(ctx, `CREATE TABLE fx_cost_at_commit (owner TEXT, cost REAL)`)
	c.exec(ctx, `CREATE TRIGGER fx_cost_probe AFTER UPDATE OF state ON async_jobs
		WHEN OLD.state = 'running' AND NEW.state <> 'running' AND NEW.child_session_id IS NOT NULL
		BEGIN INSERT INTO fx_cost_at_commit SELECT NEW.owner_session_id, cost FROM sessions WHERE id = NEW.owner_session_id; END`)

	// The first turn: spend, then runSubAgent's own transfer.
	_, err := c.env.sessions.IncrementCost(ctx, c.childID, 0.10)
	require.NoError(t, err)
	require.NoError(t, c.coord.updateParentSessionCost(ctx, c.childID, c.parentID))
	require.InDelta(t, 0.10, c.sessionCost(c.parentID), 1e-9)
	c.park()

	c.finishShell() // the Drain turn: +$0.30 on the child
	got := c.awaitRelease()
	require.Equal(t, "delegate-1", got.ToolCallID)

	require.InDelta(t, 0.40, c.sessionCost(c.parentID), 1e-9, "both turns, once")
	var atCommit float64
	require.NoError(t, c.env.conn.QueryRowContext(ctx, `SELECT cost FROM fx_cost_at_commit WHERE owner = ?`, c.parentID).Scan(&atCommit))
	require.InDelta(t, 0.40, atCommit, 1e-9, "the parent's cost is complete when the notice commits")

	// A later charge of the same child moves nothing (no double count).
	require.NoError(t, c.coord.updateParentSessionCost(ctx, c.childID, c.parentID))
	require.NoError(t, c.env.sessions.TransferChildCostToParent(ctx, c.childID, c.parentID))
	require.InDelta(t, 0.40, c.sessionCost(c.parentID), 1e-9)
	require.InDelta(t, 0.40, c.sessionCost(c.childID), 1e-9)
}

// TestChildRunEnd_ChargesTheParentItsSpend: a Stop of the delegation (the row
// goes terminal without a natural release) after the child's Drain turn spent
// $0.30 still charges the parent through the child's run end, once; a root's
// release charges nobody.
//
// Revert-check: dropping the charge from afterRelease leaves the parent at
// $0.10 and this test goes red.
func TestChildRunEnd_ChargesTheParentItsSpend(t *testing.T) {
	ctx := context.Background()
	c := newChildBGFixture(t, childBGMode{noIdle: true})
	_, err := c.env.sessions.IncrementCost(ctx, c.childID, 0.10)
	require.NoError(t, err)
	require.NoError(t, c.coord.updateParentSessionCost(ctx, c.childID, c.parentID))

	// A Drain turn spends $0.30, then the delegation is stopped: the row goes
	// terminal, and the turn's run end is the last thing that can charge it.
	_, err = c.env.sessions.IncrementCost(ctx, c.childID, 0.30)
	require.NoError(t, err)
	c.ledger.cancelSession(c.childID)
	c.coord.afterRelease(c.childID)

	require.InDelta(t, 0.40, c.sessionCost(c.parentID), 1e-9)
	c.coord.afterRelease(c.childID) // a second release charges nothing more
	require.InDelta(t, 0.40, c.sessionCost(c.parentID), 1e-9)

	// A root session's release charges nobody.
	c.coord.afterRelease(c.parentID)
	require.InDelta(t, 0.40, c.sessionCost(c.parentID), 1e-9)
	require.InDelta(t, 0, c.sessionCost(c.sessID), 1e-9)
}
