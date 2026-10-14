package agent

// R12-E round 7 (fix 2): the FULL sessionAgent Drain entry over the real
// runTurn harness. The attemptFixture provides the real sessionAgent +
// httptest provider; the BATCH debt lives in a separate cutoverEnv DB whose
// node-backed facts are bound to the FIXTURE's coordinator, and the batch
// completion is OWNED BY THE FIXTURE's session (owner_session_id =
// f.sessID in the CUTOVER DB) so the ON capture -- filtered by owner --
// actually sees it. async_jobs is DROPPED on the fixture's conn (the DB the
// fixture's own store f.store wraps, drain_attempt_fixture_test.go): any
// legacy async_jobs read anywhere in the turn (recovery: gated by
// !batchFlagEnabled, agent_turn.go; pull: the ON branch reads batch facts
// only, agent_notice_pull.go; capture: CaptureVisibleDebt over batch
// tables; decide: readTurnFactsBatch; accounting: the drainNoTurn early
// return + the batch-sourced pendingLeft) fails loudly. The provider must
// be REACHED on the one visible batch completion (the commit=true
// decision), with no error.

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// invokeDetachedAs is cutoverEnv.invokeDetached with an EXPLICIT owner: the
// fixture's session owns this root, not env.sessionID (the BatchInvocation
// carries SessionID=owner; everything else is the detached shape every AW
// scenario uses). deliverDetached still runs ElectStarted inside the
// detached delivery, so the announced debt rows are visible.
func invokeDetachedAs(t *testing.T, env *cutoverEnv, owner, callID, toolName string, leaf fantasy.AgentTool) {
	t.Helper()
	inv := env.invocation(callID)
	inv.SessionID = owner
	if _, err := env.install.Runtime.Invoke(env.ctx, inv, leaf,
		fantasy.ToolCall{ID: callID, Name: toolName, Input: `{"p":1}`}); err != nil {
		t.Fatalf("%s: detached Invoke: %v", callID, err)
	}
}

// rootIDFor is cutoverEnv.rootID scoped to an explicit owner.
func rootIDFor(t *testing.T, env *cutoverEnv, owner, callID string) string {
	t.Helper()
	var rootID string
	err := env.conn.QueryRowContext(env.ctx,
		`SELECT root_id FROM batch_nodes WHERE owner_session_id = ?1 AND tool_call_id = ?2 AND node_id = root_id`,
		owner, callID).Scan(&rootID)
	require.NoError(t, err, "no root row reserved for %s (owner %s)", callID, owner)
	return rootID
}

// nodeNoticeFor is cutoverEnv.nodeNotice scoped to an explicit owner.
func nodeNoticeFor(t *testing.T, env *cutoverEnv, owner, nodeID string) string {
	t.Helper()
	var delivery string
	err := env.conn.QueryRowContext(env.ctx,
		`SELECT delivery FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
		nodeID, owner).Scan(&delivery)
	require.NoError(t, err, "no batch node %s (owner %s)", nodeID, owner)
	return delivery
}

func TestBatchDrainTurnEntryReachesProviderON(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")

	// The real runTurn harness: sessionAgent + probe provider (its default
	// handler answers one text step; requests counts provider calls).
	f := newAttemptFixture(t, "batch-drain-entry", attemptFixtureOpts{noIdle: true})
	// Delivered shape (round 7): the ledger keeps the fixture's OWN store
	// (built over f.env.conn at drain_attempt_fixture_test.go), and
	// async_jobs is DROPPED on that same conn BEFORE the turn. Store/conn
	// identity: f.store == f.ledger.store wraps f.env.conn; the drop
	// provably precedes drainRun and every legacy read of the turn fails
	// with "no such table". Store.Close after the drop is safe: its only
	// async_jobs touch is DeleteAsyncHostIfNoJobs (host_lock.go HostIdentity
	// .Close), whose error is swallowed while the lock still releases.
	_, err := f.env.conn.ExecContext(context.Background(), `DROP TABLE async_jobs`)
	require.NoError(t, err)

	// The batch DB: one real settled, DELIVERED batch completion owned by
	// the FIXTURE's session. A plain countingTool leaf (no gates): the root
	// settles instantly, no release bookkeeping.
	fast := &countingTool{name: "view"}
	env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, fast)
	invokeDetachedAs(t, env, f.sessID, "call-entry", "view", fast)
	rootID := rootIDFor(t, env, f.sessID, "call-entry")
	require.Equal(t, "completed", env.rootTerminal(rootID))
	// Pull with the OWNER f.sessID (BatchPullNotices is owner-scoped): the
	// completion notice delivers before the Drain turn.
	require.Eventually(t, func() bool {
		pulled, err := env.store.BatchPullNotices(env.ctx, env.messages, f.sessID, buildBatchNoticeMessageParams)
		require.NoError(t, err)
		return len(pulled) >= 1
	}, 5*time.Second, 5*time.Millisecond, "the batch pull never delivered the completion for %s", f.sessID)
	require.Equal(t, "done", nodeNoticeFor(t, env, f.sessID, rootID),
		"the completion is delivered before the Drain turn")
	// Direct oracle: the bound facts' owner-scoped visible capture is
	// NONEMPTY for f.sessID -- exactly what the turn's ON capture reads.
	bound := NewBatchCutoverNodeFacts(env.store, env.live, env.conn)
	ictx, icancel := context.WithTimeout(env.ctx, drainAccountBudget())
	isnap, ierr := bound.CaptureVisibleDebt(ictx, f.sessID)
	icancel()
	require.NoError(t, ierr)
	require.False(t, isnap.Empty(), "the fixture owner's delivered completion is visible batch debt")

	BindCoordinatorBatchCutoverFacts(f.coord, NewBatchCutoverNodeFacts(env.store, env.live, env.conn))
	t.Cleanup(func() { coordinatorBatchCutoverFactsBindings.Delete(f.coord) })

	// FIRST ASSERTION (static trace in the report): the turn reaches the
	// provider. Trace: sa.Run -> runTurn with call.IsDrain -> the att block
	// (agent_turn.go) -> batchFlagEnabled() && coordinatorBatchCutoverFacts
	// != nil (bound above) -> CaptureVisibleDebt over batch_nodes filtered
	// by owner f.sessID (the delivered row: BatchOutboxDebtNodes keeps
	// delivery IN ('pending','done') with wake=1, reacted=0) ->
	// hasVisibleDebt=true -> decideDrainTurn -> arbiterVerdict ->
	// readTurnFactsBatch (no async_jobs read) -> a FRESH gate over pending
	// debt is an allow (gateShut false) -> commit=true -> the provider
	// Stream runs.
	res, runErr := f.drainRun(context.Background())
	require.NoError(t, runErr, "the Drain turn completes over batch-only debt with async_jobs dropped")
	require.NotNil(t, res)
	require.GreaterOrEqual(t, f.requests.Load(), int32(1), "the provider was reached on the committed turn")

	// The OFF twin cannot share the fixture (the flag is process-global and
	// the batch DB binding is ON): the legacy-only twin lives in
	// drain_attempt_test.go's existing counted-attempt tests.
}
