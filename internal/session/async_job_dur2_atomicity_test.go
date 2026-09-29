// DUR-2 atomicity coverage (docs/plans/2026-09-28-async-phase4-durable-core.md
// sec.3.2/sec.4: "terminal transition, delivery='pending' and wake by cause
// -- one transaction"). Transition (async_job_store.go) already proves the
// WON direction (TestAsyncJobStore_TransitionWonThenLostThenGone: a
// committed transition sets state/delivery/wake together). This file proves
// the fault-injection direction the design doc's sec.6 asks for: a
// transaction that runs the SAME single-statement CAS but is rolled back
// instead of committed -- standing in for a crash/disk failure between the
// UPDATE and the commit -- leaves ALL THREE fields (state, delivery, wake)
// exactly as they were, never a partial write of one or two of them. Since
// TransitionAsyncJobTerminalPreserveVoid sets state/delivery/wake/reacted in
// one SQL UPDATE (internal/db/sql/async_jobs.sql), this is the only place a
// partial write could ever come from; if the row is untouched. after a
// rollback, then a WON commit and a LOST/GONE outcome are the only two
// possible worlds, exactly as DUR-1/DUR-2 claim.
package session

import (
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// TestTransitionAsyncJobTerminal_RolledBackTransactionChangesNothing is
// DUR-2's fault-injection proof: run the exact CAS query Transition uses,
// inside a transaction that is then rolled back (not committed) -- the row,
// read back through a completely separate connection, must show its
// ORIGINAL state/delivery/wake, not a mix where e.g. state changed but
// delivery/wake did not (which would be possible only if these were
// separate statements/transactions, which they are not).
//
// REVERT CHECK: temporarily changed `tx.Rollback()` below to `tx.Commit()`
// -- the test FAILED (`after.State` was "completed", not "running") because
// the CAS's effects were then genuinely visible, proving the assertions
// below actually observe whether the transaction committed. Restored
// `tx.Rollback()`; re-ran, passed.
func TestTransitionAsyncJobTerminal_RolledBackTransactionChangesNothing(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	claimed, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "echo hi"})
	require.NoError(t, err)
	require.Equal(t, "running", claimed.Row.State)
	require.Equal(t, "none", claimed.Row.Delivery)
	require.EqualValues(t, 0, claimed.Row.Wake)

	tx, err := store.sqlDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	txq := db.New(tx)

	_, err = txq.TransitionAsyncJobTerminalPreserveVoid(ctx, db.TransitionAsyncJobTerminalPreserveVoidParams{
		State:          "completed",
		NoticeKind:     "",
		ResultSummary:  sql.NullString{String: "ok", Valid: true},
		ResultIsError:  sql.NullInt64{Valid: true},
		Delivery:       "pending",
		Wake:           1,
		Reacted:        0,
		UpdatedAt:      claimed.Row.UpdatedAt + 1,
		OwnerSessionID: "owner-1",
		ToolCallID:     "call-1",
	})
	require.NoError(t, err, "the CAS statement itself must succeed inside the transaction")

	// Simulate the commit never happening (crash/disk failure/process death
	// right before it) -- roll back instead.
	require.NoError(t, tx.Rollback())

	// Read back through the store's own Get (a separate query, same
	// connection pool but outside any transaction) -- if the UPDATE's
	// effects were visible here, the rollback did not actually undo them.
	after, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "running", after.State, "an uncommitted transition must not change state")
	require.Equal(t, "none", after.Delivery, "an uncommitted transition must not change delivery")
	require.EqualValues(t, 0, after.Wake, "an uncommitted transition must not change wake")
	require.EqualValues(t, 0, after.Reacted, "an uncommitted transition must not change reacted")
}
