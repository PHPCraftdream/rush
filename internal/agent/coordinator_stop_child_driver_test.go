// R7B-2: stopping a delegation whose child is parked on the child's OWN async
// job must release the child's driver record and allowlist entry. Nothing
// re-checked the child after the stop (the child is no longer parked: its job
// and the delegation went terminal; the idle mailbox fires no release hook),
// so subAgentDrivers.byChild[child] lived as long as the process and
// agentFor(child) kept routing to the idle task agent.
package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestStop_ReleasesTheChildDriverOfAParkedDelegation: the delegation is parked
// on the child's own running ledger job; each way of stopping it (Stop on the
// parent, Stop on the child, a Rerun that voided the delegation) leaves the
// child with no driver record, so agentFor(child) is the ordinary agent again.
//
// Revert-check: dropping the recheckChild loop from stopTree leaves the
// driver registered in all three cases.
func TestStop_ReleasesTheChildDriverOfAParkedDelegation(t *testing.T) {
	stops := []struct {
		name string
		stop func(c *childBGFixture)
	}{
		{"Stop on the parent", func(c *childBGFixture) { c.coord.Cancel(c.parentID) }},
		{"Stop on the child", func(c *childBGFixture) { c.coord.Cancel(c.childID) }},
		{"Rerun voiding the delegation", func(c *childBGFixture) {
			c.coord.StopRerunJobs(context.Background(), c.parentID, []session.VoidedAsyncJob{{
				ToolCallID: "delegate-1", State: "running", HostID: c.store.HostID(), ChildSessionID: c.childID,
			}})
		}},
	}
	for _, tc := range stops {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := newChildBGFixture(t, childBGMode{})
			c.coord.currentAgent = &mockSessionAgent{}
			// The child's own async job replaces its held shell as the open work.
			require.NoError(t, c.mgr.Kill(ctx, c.held.ID))
			_, _, err := c.ledger.Start(c.childID, "child-job", "sleep 30", "bash", "", false, false, nil, func() {})
			require.NoError(t, err)
			c.ledger.acknowledged(jobOf(c.ledger, c.childID, "child-job"))
			c.park()
			require.True(t, c.ledger.running(c.childID))
			_, registered := c.coord.subAgentDrivers.get(c.childID)
			require.True(t, registered)

			tc.stop(c)

			_, registered = c.coord.subAgentDrivers.get(c.childID)
			require.False(t, registered, "the stopped child's driver record must be released")
			require.Same(t, c.coord.currentAgent, c.coord.agentFor(c.childID), "the child routes to the ordinary agent again")
			require.False(t, c.ledger.hasParked())
		})
	}
}

// TestStop_StaleExecutorReleasesTheChildDriverItsShellHeld (R8B-4): Stop lands
// while the child's async bash executor is still inside its fixed fast-failure
// sleep, so the child's shell sits in the table. The stop re-check finds the
// child not drained (the shell runs) and leaves the driver; the executor then
// kills the shell and calls finish for a job the stop already dropped. finish
// must re-check the owner, or the driver record and allowlist entry live as
// long as the process.
//
// Revert-check: dropping the recheckChild from finish's stale-job branch leaves
// the driver registered after the executor returns.
func TestStop_StaleExecutorReleasesTheChildDriverItsShellHeld(t *testing.T) {
	stops := []struct {
		name string
		stop func(c *childBGFixture)
	}{
		{"Stop on the parent", func(c *childBGFixture) { c.coord.Cancel(c.parentID) }},
		{"Stop on the child", func(c *childBGFixture) { c.coord.Cancel(c.childID) }},
	}
	for _, tc := range stops {
		t.Run(tc.name, func(t *testing.T) {
			c := newChildBGFixture(t, childBGMode{})
			c.coord.currentAgent = &mockSessionAgent{}
			job, _, err := c.ledger.Start(c.childID, "child-job", "sleep 30", "bash", "", false, false, nil, func() {})
			require.NoError(t, err)
			c.ledger.acknowledged(job)
			c.park()
			require.Equal(t, 1, c.mgr.ActiveOwned(c.childID), "the executor's shell is in the table")

			tc.stop(c)

			_, registered := c.coord.subAgentDrivers.get(c.childID)
			require.True(t, registered, "the shell still runs: the stop's own re-check cannot release the child yet")
			require.False(t, c.ledger.running(c.childID), "the stop dropped the job")

			// The executor wakes: awaitShell sees its ctx done, kills the shell, finish runs.
			require.NoError(t, c.mgr.KillOwned(t.Context(), c.childID, c.held.ID))
			c.ledger.finish(job, jobResult{content: "killed"})

			_, registered = c.coord.subAgentDrivers.get(c.childID)
			require.False(t, registered, "the stale executor's finish releases the child's driver")
			require.False(t, c.ledger.hasParked())
		})
	}
}
