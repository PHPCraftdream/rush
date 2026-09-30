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
