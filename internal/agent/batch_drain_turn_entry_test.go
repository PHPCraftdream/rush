package agent

// R12-E round 6 (fix 2b): the FULL sessionAgent Drain entry over the real
// runTurn harness. The attemptFixture provides the real sessionAgent +
// httptest provider; the BATCH debt lives in a separate cutoverEnv DB whose
// node-backed facts are bound to the FIXTURE's coordinator, and async_jobs
// is DROPPED on the fixture's conn: any legacy async_jobs read anywhere in
// the turn (recovery, pull, capture, decide, accounting) fails loudly. The
// provider must be REACHED on zero legacy debt + one visible batch
// completion (the commit=true decision), with no error.

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func TestBatchDrainTurnEntryReachesProviderON(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")

	// The real runTurn harness: sessionAgent + probe provider (its default
	// handler answers one text step; requests counts provider calls).
	f := newAttemptFixture(t, "batch-drain-entry", attemptFixtureOpts{noIdle: true})
	// Move the ledger's legacy store off the fixture's conn (a headless
	// store the ON path never needs), then DROP async_jobs on the fixture
	// conn: any legacy read in this turn fails with "no such table".
	f.ledger.store = newTestAsyncJobStore(t)
	_, err := f.env.conn.ExecContext(context.Background(), `DROP TABLE async_jobs`)
	require.NoError(t, err)

	// The batch DB: one real settled, DELIVERED batch completion (visible
	// debt only), bound to the fixture coordinator's fact source.
	leaf := newGatedLeaf("view")
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, leaf)
	releaseAllAt(t, leaf)
	env.invokeDetached("call-entry", "view", leaf)
	rootID := env.rootID("call-entry")
	require.Equal(t, "completed", env.rootTerminal(rootID))
	env.requireBatchPull(1)
	delivery, _, _, _ := env.nodeNotice(rootID)
	require.Equal(t, "done", delivery, "the completion is delivered before the Drain turn")
	BindCoordinatorBatchCutoverFacts(f.coord, NewBatchCutoverNodeFacts(env.store, env.live, env.conn))
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(f.coord) })

	// FIRST ASSERTION (static trace in the report): the turn reaches the
	// provider. Trace: sa.Run -> runTurn with call.IsDrain -> the att block
	// (agent_turn.go) -> batchFlagEnabled() && coordinatorBatchCutoverFacts
	// != nil (bound above) -> CaptureVisibleDebt over batch_nodes (the
	// delivered row: BatchOutboxDebtNodes keeps delivery IN
	// ('pending','done') with wake=1, reacted=0) -> hasVisibleDebt=true ->
	// decideDrainTurn -> arbiterVerdict -> readTurnFactsBatch (no
	// async_jobs read) -> a FRESH gate over pending debt is an allow
	// (gateShut false) -> commit=true -> the provider Stream runs.
	res, runErr := f.drainRun(context.Background())
	require.NoError(t, runErr, "the Drain turn completes over batch-only debt with async_jobs dropped")
	require.NotNil(t, res)
	require.GreaterOrEqual(t, f.requests.Load(), int32(1), "the provider was reached on the committed turn")

	// The OFF twin cannot share the fixture (the flag is process-global and
	// the batch DB binding is ON): the legacy-only twin lives in
	// drain_attempt_test.go's existing counted-attempt tests.
}
