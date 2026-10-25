// Class-A fix: the live scope comes from the
// bound node-backed fact source (batch_nodes), never from the legacy
// async_jobs ledger. Invariant: a live batch root owned by the session
// keeps CLIActiveWork true (a quota-hit `rush run` must not exit while
// batch work runs); after the root settles it reads false. Revert-check:
// removing the flag-ON branch from CLIActiveWork makes the seeded live
// root read false through the empty ledger and turns this red.
package agent

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func TestCLIActiveWork_BatchLiveRootKeepsCLIActiveFlagOn(t *testing.T) {
	// One held leaf: the root stays running until the gate releases.
	leaf := newGatedLeaf("view")
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, leaf)
	releaseAllAt(t, leaf)

	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)

	// Seed one live batch root (running) owned by env.sessionID with a
	// task child; no async_jobs rows exist for the session.
	env.invokeDetached("call-active-batch", "view", leaf)
	leaf.waitStarted(t, "call-active-batch")
	rootID := env.rootID("call-active-batch")

	active, err := coord.CLIActiveWork(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.True(t, active, "a live batch root must keep a quota-hit rush run waiting")

	// Terminal transition through the same gated-leaf API the awaited
	// tests use: release the gate, wait for the root to settle.
	leaf.letGo("call-active-batch")
	require.Equal(t, "completed", env.rootTerminal(rootID))
	require.Eventually(t, func() bool {
		active, err := coord.CLIActiveWork(env.ctx, env.sessionID)
		return err == nil && !active
	}, 5*time.Second, 5*time.Millisecond, "the settled root releases the CLI")

	// Sanity: an UNBOUND flag-ON coordinator keeps the legacy ledger path
	// (its separate test async_jobs store is empty), so CLIActiveWork
	// deterministically answers idle with no error.
	unbound := newCutoverCoordinator(t, env)
	active, err = unbound.CLIActiveWork(env.ctx, env.sessionID)
	require.NoError(t, err)
	require.False(t, active, "the unbound coordinator reads the empty legacy ledger as idle")
}
