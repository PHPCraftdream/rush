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

// TestOwnWorkBatch_LiveOwnCommandFlagOn: with installed facts and a live own
// batch command, the guard's ownWork is true (batch facts are the only
// source; the deleted in-process ledger map no longer exists).
// First production-facing assertion: the require.NoError inside
// batchQuestionFixture's connection setup.
// FIRST REGRESSION-SENSITIVE ASSERTION: armOwnWork true — the call-site
// mutation "drop the batchOwnWorkFactsInstalled gate" makes armProgressGuard
// arm ownWork from the unknown (unbound) helper and flips it to false.
func TestOwnWorkBatch_LiveOwnCommandFlagOn(t *testing.T) {
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const owner = "sess-ownwork-cmd"
	createSessionRow(t, db.New(conn), ctx, owner)
	seedBatchDelegation(t, conn, owner, "call-ownwork-cmd", "", "command", "running", false)
	sa := &sessionAgent{coord: coord}

	require.True(t, sa.ownWorkBatch(ctx, owner), "a live own batch command keeps ownWork true (there is no in-process map left to be empty: the facts are the only source)")

	require.True(t, armOwnWork(t, sa, owner), "armProgressGuard must arm ownWork from the batch facts")
}

// TestOwnWorkBatch_HeldQuestionMatchesCLIActiveWork: a live held-question
// delegation answers per the same rule as CLIActiveWork — active until the
// child_question marker notice is durably committed, released after.
// First production-facing assertion: the require.NoError inside
// batchQuestionFixture's connection setup.
// FIRST REGRESSION-SENSITIVE ASSERTION: line 81 — deleting
// batchActiveAfterQuestionPause's committed-notice exclusion (answering true
// whenever live work exists) makes the post-commit False assertion fail;
// dropping the question-pause logic entirely (plain LiveWorkSummary) fails
// the same assertion from the other direction. The CLIActiveWork assertions
// on lines 64/69 are the parity premise, not the regression catch.
func TestOwnWorkBatch_HeldQuestionMatchesCLIActiveWork(t *testing.T) {
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const owner = "sess-ownwork-q"
	createSessionRow(t, db.New(conn), ctx, owner)
	createSessionRow(t, db.New(conn), ctx, "sess-ownwork-q-child")
	_, nodeID := seedDelegation(t, conn, owner, "call-ownwork-q", "sess-ownwork-q-child", 7)
	seedQuestionStop(t, coord, "sess-ownwork-q-child")
	sa := &sessionAgent{coord: coord}

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
// First production-facing assertion: the require.NoError inside
// batchQuestionFixture's connection setup.
// FIRST REGRESSION-SENSITIVE ASSERTION: line 102 — a predicate that answers
// true unconditionally (e.g. the conservative fail-closed branch leaked into
// the happy path) fails the terminal and empty assertions.
func TestOwnWorkBatch_TerminalWorkFalse(t *testing.T) {
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const owner = "sess-ownwork-done"
	const idle = "sess-ownwork-idle"
	createSessionRow(t, db.New(conn), ctx, owner)
	createSessionRow(t, db.New(conn), ctx, idle)
	seedBatchDelegation(t, conn, owner, "call-ownwork-done", "", "command", "completed", false)
	sa := &sessionAgent{coord: coord}

	require.False(t, sa.ownWorkBatch(ctx, owner), "terminal batch work is not live own work")
	require.False(t, sa.ownWorkBatch(ctx, idle), "a session with no batch work reads idle")
}

// TestOwnWorkBatch_UnboundFactsIdle: with the cutover facts UNBOUND the
// guard reads idle even though live batch rows exist -- an unbound helper
// must never answer "live" (the deleted legacy-ledger fallback is gone; the
// conservative "unknown = live" rule belongs to the DURABLE consumers, which
// do their own reads, exactly like CLIActiveWork's unbound rule).
// First production-facing assertion: the require.NoError inside
// batchQuestionFixture's connection setup.
// FIRST REGRESSION-SENSITIVE ASSERTION: armOwnWork false -- the call-site
// mutation "use ownWorkBatch's happy path ignoring the installed-facts gate"
// answers from the unbound helper as if live and flips this to true.
func TestOwnWorkBatch_UnboundFactsIdle(t *testing.T) {
	_, _, conn, _, ctx := batchQuestionFixture(t)
	coord := newBatchRuntimeCoordinator()
	coord.messages = message.NewService(db.New(conn))
	// Deliberately NO cutover-facts binding. Seed one live batch row through
	// the constraint-valid helper to prove the unbound path ignores it.
	const owner = "sess-ownwork-unbound"
	createSessionRow(t, db.New(conn), ctx, owner)
	seedBatchDelegation(t, conn, owner, "call-ownwork-unbound-batch", "", "command", "running", false)
	sa := &sessionAgent{coord: coord}

	require.False(t, sa.ownWorkBatch(ctx, owner), "an unbound coordinator has no batch facts: the helper answers false")
	require.False(t, armOwnWork(t, sa, owner), "the unbound arm must not invent live own work")
}
