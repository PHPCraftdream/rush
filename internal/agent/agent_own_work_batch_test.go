package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// armOwnWork drives the real armProgressGuard path for a synthetic turn
// stream and returns the guard's ownWork snapshot.
func armOwnWork(t *testing.T, sa *sessionAgent, sessionID string) bool {
	t.Helper()
	ts := &turnStream{a: sa, call: SessionAgentCall{SessionID: sessionID}}
	ctx := WithCallOrigin(context.Background(), message.OriginCLI)
	var out []fantasy.AgentTool
	var pinned []fantasy.AgentTool
	ts.armProgressGuard(ctx, pinned, &out)
	return ts.progress.guard.ownWork
}

// TestOwnWorkBatch_LiveOwnCommandFlagOn: ON with installed facts and a live
// own batch command, the guard's ownWork is true even though the legacy
// ledger is empty.
// First production-facing assertion: the require.NoError after db.Connect in
// newTestAsyncJobStoreWithDataDir (coordinator_scope_recovery_test.go:26-27).
// FIRST REGRESSION-SENSITIVE ASSERTION: line 48 (armOwnWork true) — the
// call-site mutation "drop the batchOwnWorkFactsInstalled gate and the
// batchFlagEnabled branch (revert to the ledger-only read)" makes
// armProgressGuard arm ownWork from the empty ledger and flips it to false;
// assertions on lines 38-39 still pass under that mutation (the helper
// itself is not reverted by it).
func TestOwnWorkBatch_LiveOwnCommandFlagOn(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const owner = "sess-ownwork-cmd"
	createSessionRow(t, db.New(conn), ctx, owner)
	seedBatchDelegation(t, conn, owner, "call-ownwork-cmd", "", "command", "running", false)
	sa := &sessionAgent{asyncJobs: coord.asyncJobs}

	require.False(t, coord.asyncJobs.running(owner), "the ON ledger map must be empty (the premise of the gap)")
	require.True(t, sa.ownWorkBatch(ctx, owner), "a live own batch command keeps ownWork true")

	require.True(t, armOwnWork(t, sa, owner), "armProgressGuard must arm ownWork from the batch facts")
}

// TestOwnWorkBatch_HeldQuestionMatchesCLIActiveWork: a live held-question
// delegation answers per the same rule as CLIActiveWork — active until the
// child_question marker notice is durably committed, released after.
// First production-facing assertion: the require.NoError after db.Connect in
// newTestAsyncJobStoreWithDataDir (coordinator_scope_recovery_test.go:26-27).
// FIRST REGRESSION-SENSITIVE ASSERTION: line 81 — deleting
// batchActiveAfterQuestionPause's committed-notice exclusion (answering true
// whenever live work exists) makes the post-commit False assertion fail;
// dropping the question-pause logic entirely (plain LiveWorkSummary) fails
// the same assertion from the other direction. The CLIActiveWork assertions
// on lines 64/69 are the parity premise, not the regression catch.
func TestOwnWorkBatch_HeldQuestionMatchesCLIActiveWork(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const owner = "sess-ownwork-q"
	createSessionRow(t, db.New(conn), ctx, owner)
	createSessionRow(t, db.New(conn), ctx, "sess-ownwork-q-child")
	_, nodeID := seedDelegation(t, conn, owner, "call-ownwork-q", "sess-ownwork-q-child", 7)
	seedQuestionStop(t, coord, "sess-ownwork-q-child")
	sa := &sessionAgent{asyncJobs: coord.asyncJobs}

	cliActive, err := coord.CLIActiveWork(ctx, owner)
	require.NoError(t, err)
	require.True(t, cliActive, "premise: the paused child's uncommitted notice keeps the CLI active")
	require.True(t, sa.ownWorkBatch(ctx, owner), "ownWork follows the CLIActiveWork rule (uncommitted: true)")

	commitMarkerNotice(t, conn, owner, nodeID, "sess-ownwork-q-child", 7)
	cliActive, err = coord.CLIActiveWork(ctx, owner)
	require.NoError(t, err)
	require.False(t, cliActive, "premise: the committed marker releases the CLI")
	require.False(t, sa.ownWorkBatch(ctx, owner), "ownWork follows the CLIActiveWork rule (committed: false)")
}

// TestOwnWorkBatch_TerminalWorkFalse: terminal batch work (and no work at
// all) reads false.
// First production-facing assertion: the require.NoError after db.Connect in
// newTestAsyncJobStoreWithDataDir (coordinator_scope_recovery_test.go:26-27).
// FIRST REGRESSION-SENSITIVE ASSERTION: line 102 — a predicate that answers
// true unconditionally (e.g. the conservative fail-closed branch leaked into
// the happy path) fails the terminal and empty assertions.
func TestOwnWorkBatch_TerminalWorkFalse(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const owner = "sess-ownwork-done"
	const idle = "sess-ownwork-idle"
	createSessionRow(t, db.New(conn), ctx, owner)
	createSessionRow(t, db.New(conn), ctx, idle)
	seedBatchDelegation(t, conn, owner, "call-ownwork-done", "", "command", "completed", false)
	sa := &sessionAgent{asyncJobs: coord.asyncJobs}

	require.False(t, sa.ownWorkBatch(ctx, owner), "terminal batch work is not live own work")
	require.False(t, sa.ownWorkBatch(ctx, idle), "a session with no batch work reads idle")
}

// TestOwnWorkBatch_UnboundFactsKeepLedger: with the cutover facts UNBOUND the
// guard keeps the legacy ledger expression under BOTH flag values — an
// unbound helper must never hide a live legacy ledger entry, and the ON arm
// falls back to it exactly like CLIActiveWork's unbound rule. Runs the same
// body under RUSH_BATCH_RUNTIME=0 and =1 so the flag-ON CI suite (which
// inherits RUSH_BATCH_RUNTIME=1) is covered deterministically, not by the
// inherited environment.
// First production-facing assertion (both subtests): the require.NoError
// after db.Connect in newTestAsyncJobStoreWithDataDir
// (coordinator_scope_recovery_test.go:26-27).
// FIRST REGRESSION-SENSITIVE ASSERTION (both subtests): line 146 — the
// call-site mutation "use ownWorkBatch whenever the flag is ON, ignoring the
// installed-facts gate" makes the ON subtest's armProgressGuard answer from
// the unbound helper (false) and flips this to false; the OFF subtest then
// still passes, which is why the ON subtest exists.
func TestOwnWorkBatch_UnboundFactsKeepLedger(t *testing.T) {
	for _, tc := range []struct{ name, flag string }{{"flag_off", "0"}, {"flag_on", "1"}} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RUSH_BATCH_RUNTIME", tc.flag)
			store, _, conn := newTestAsyncJobStoreWithDataDir(t)
			coord := &coordinator{subAgentDrivers: newSubAgentDriverRegistry(), messages: message.NewService(db.New(conn))}
			l := newWorkLedger(coord.notifyAsyncCompletion)
			l.store = store
			l.coord = coord
			coord.asyncJobs = l
			// Deliberately NO cutover-facts binding. Seed one live batch row
			// through the constraint-valid helper to prove the unbound path
			// ignores it.
			const owner = "sess-ownwork-unbound"
			createSessionRow(t, db.New(conn), context.Background(), owner)
			seedBatchDelegation(t, conn, owner, "call-ownwork-unbound-batch", "", "command", "running", false)
			sa := &sessionAgent{asyncJobs: coord.asyncJobs}

			require.False(t, sa.ownWorkBatch(context.Background(), owner), "an unbound coordinator has no batch facts: the helper answers false")

			_, existing, err := l.Start(owner, "call-ownwork-unbound-ledger", "call-ownwork-unbound-ledger", "bash", "", false, false, nil, func() {})
			require.NoError(t, err)
			require.False(t, existing)
			require.True(t, l.running(owner), "premise: the legacy ledger entry is live")

			require.True(t, armOwnWork(t, sa, owner), "the unbound arm still reads the legacy ledger")
		})
	}
}
