package session

// AsyncJobStore coverage (docs/plans/2026-09-28-async-phase4-durable-core.md
// sec.5 step 2): Claim's idempotency and ASYNC-01 conflict-before-"started"
// refusal, Transition's three-way Won/Lost/Gone outcome, and the layering
// boundary that the retry-with-backoff loop lives in the CALLER (workLedger,
// internal/agent), not in the store itself -- a busy DB here surfaces as a
// plain error, not a silent retry.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// newTestStore opens a real, migrated SQLite DB in a fresh temp dir and
// returns an AsyncJobStore plus a *db.Queries for seeding sessions rows
// (async_jobs.owner_session_id has an FK to sessions) via the existing
// seedSession helper (host_lock_test.go).
func newTestStore(t *testing.T) (*AsyncJobStore, *db.Queries, context.Context) {
	t.Helper()
	dataDir := t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	store := NewAsyncJobStore(conn, dataDir, 999, "test")
	// Release the host's OS lock file before t.TempDir()'s own cleanup tries
	// to remove the directory -- otherwise Windows refuses to delete a file
	// this same process still holds open.
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store, db.New(conn), ctx
}

func TestAsyncJobStore_ClaimIsIdempotent(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	p := ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "echo hi"}
	first, err := store.Claim(ctx, p)
	require.NoError(t, err)
	require.False(t, first.Existing)
	require.Equal(t, "running", first.Row.State)
	require.NotEmpty(t, store.HostID(), "the first Claim must have lazily registered a host")

	second, err := store.Claim(ctx, p)
	require.NoError(t, err)
	require.True(t, second.Existing, "the same (owner, tool_call_id, input) must be reported as an idempotent repeat")
	require.Equal(t, first.Row.ToolCallID, second.Row.ToolCallID)
}

func TestAsyncJobStore_ClaimDifferentInputIsRefused(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "echo hi"})
	require.NoError(t, err)

	_, err = store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "rm -rf /"})
	require.Error(t, err)
	var mismatch *ErrAsyncJobInputMismatch
	require.ErrorAs(t, err, &mismatch)
	require.Equal(t, "call-1", mismatch.ToolCallID)
}

// TestAsyncJobStore_ClaimChildSessionConflictRefusedBeforeStarted pins
// ASYNC-01 (doc sec.3.8): a delegation naming a child session id a RUNNING
// row already claims is refused with a typed error BEFORE the tool ever
// reports "started" -- every conflict is a refusal in this step (dead-host
// recovery of the conflicting row is step 5/6).
func TestAsyncJobStore_ClaimChildSessionConflictRefusedBeforeStarted(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "parent-1"))
	require.NoError(t, seedSession(ctx, q, "parent-2"))

	_, err := store.Claim(ctx, ClaimParams{
		Owner: "parent-1", ToolCallID: "call-1", Kind: JobKindAgent, Input: "do work", ChildSessionID: "child-1",
	})
	require.NoError(t, err)

	// A second, DIFFERENT delegation (parent-2, a fresh tool_call_id) naming
	// the SAME child session id must be refused -- it never reaches
	// ClaimAsyncJob's insert, so no row for it exists and no "started" is
	// ever reported.
	_, err = store.Claim(ctx, ClaimParams{
		Owner: "parent-2", ToolCallID: "call-2", Kind: JobKindAgent, Input: "do work too", ChildSessionID: "child-1",
	})
	require.Error(t, err)
	var busy *ErrAsyncChildSessionBusy
	require.ErrorAs(t, err, &busy)
	require.Equal(t, "child-1", busy.ChildSessionID)
	require.Contains(t, busy.Error(), "wait for its result")

	// The refused call must not have left a partial row of its own.
	_, err = store.q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: "parent-2", ToolCallID: "call-2"})
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestAsyncJobStore_ClaimChildSessionConflictWithDeadHostIsRecoveredAndStarted
// pins step 6's ASYNC-01 extension (doc sec.3.8): a conflicting RUNNING row
// whose host is provably DEAD is recovered (interrupted) in the SAME claim
// attempt and the new delegation starts immediately, instead of refusing.
//
// Revert-check performed: reverted Claim to always refuse on any conflict
// (deleted the recovery-and-retry loop, restored the single-attempt body).
// This test FAILED (ErrAsyncChildSessionBusy instead of a fresh running
// row). Restored the step-6 version; re-ran, passed. Diffed
// async_job_store.go against the restored version: byte-identical.
func TestAsyncJobStore_ClaimChildSessionConflictWithDeadHostIsRecoveredAndStarted(t *testing.T) {
	t.Parallel()
	a, b, ctx := twoConnStores(t, "parent-0", "parent-1", "parent-2")

	// Force B's host to lazily register (and run its OWN one-time
	// first-registration dead-host sweep, async_job_store.go's ensureHost)
	// BEFORE A ever crashes -- at this point A is alive, so that sweep finds
	// nothing to recover. This isolates the assertion below to Claim's OWN
	// retry-loop recovery (this step's new code), not the pre-existing
	// first-registration sweep incidentally doing the same job first.
	_, err := b.Claim(ctx, ClaimParams{Owner: "parent-0", ToolCallID: "warmup", Kind: JobKindCommand, Input: "echo warm"})
	require.NoError(t, err)

	first, err := a.Claim(ctx, ClaimParams{
		Owner: "parent-1", ToolCallID: "call-1", Kind: JobKindAgent, Input: "do work", ChildSessionID: "child-1",
	})
	require.NoError(t, err)
	require.NoError(t, a.MarkAnnounced(ctx, "parent-1", "call-1"))
	deadHostID := first.Row.HostID
	require.NoError(t, a.SimulateCrashForTest(), "host A must appear dead to host B below")

	second, err := b.Claim(ctx, ClaimParams{
		Owner: "parent-2", ToolCallID: "call-2", Kind: JobKindAgent, Input: "do work too", ChildSessionID: "child-1",
	})
	require.NoError(t, err, "a conflict with a PROVABLY DEAD host must be recovered and the claim retried, not refused")
	require.False(t, second.Existing)
	require.Equal(t, "running", second.Row.State)

	oldRow, err := b.Get(ctx, "parent-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "interrupted", oldRow.State, "the dead host's conflicting row must be recovered in the same attempt")
	require.Equal(t, deadHostID, oldRow.HostID)
}

// TestAsyncJobStore_ClaimChildSessionConflictWithLiveHostStaysRefused pins
// the negative half of the same rule: a conflict whose host is LIVE (this
// test's own store, i.e. never provably dead) is never recovered, and stays
// refused even after Claim's one internal retry.
func TestAsyncJobStore_ClaimChildSessionConflictWithLiveHostStaysRefused(t *testing.T) {
	t.Parallel()
	a, b, ctx := twoConnStores(t, "parent-1", "parent-2")

	_, err := a.Claim(ctx, ClaimParams{
		Owner: "parent-1", ToolCallID: "call-1", Kind: JobKindAgent, Input: "do work", ChildSessionID: "child-1",
	})
	require.NoError(t, err)
	// Host A is never crashed -- ProbeHost from B's perspective must observe
	// it alive (the lock is still held), so B's claim stays refused.
	_, err = b.Claim(ctx, ClaimParams{
		Owner: "parent-2", ToolCallID: "call-2", Kind: JobKindAgent, Input: "do work too", ChildSessionID: "child-1",
	})
	require.Error(t, err)
	var busy *ErrAsyncChildSessionBusy
	require.ErrorAs(t, err, &busy)
	require.Equal(t, "child-1", busy.ChildSessionID)

	row, err := b.Get(ctx, "parent-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "running", row.State, "a live host's row must never be touched by the other side's failed claim")
}

// TestAsyncJobStore_ClaimIdempotentRepeatAnswersByRowState_Terminal pins
// ASYNC-01's third bullet: a repeat of the SAME call (same owner/tool_call/
// input) after the row has already reached a TERMINAL state answers with
// that terminal row (Existing=true), never a second executor and never an
// error -- the running-state case is TestAsyncJobStore_ClaimIsIdempotent.
func TestAsyncJobStore_ClaimIdempotentRepeatAnswersByRowState_Terminal(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	p := ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "echo hi"}
	first, err := store.Claim(ctx, p)
	require.NoError(t, err)
	require.False(t, first.Existing)

	won, err := store.Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "call-1", State: "completed", ResultSummary: "ok", Wake: true,
	})
	require.NoError(t, err)
	require.Equal(t, TransitionWon, won.Outcome)

	repeat, err := store.Claim(ctx, p)
	require.NoError(t, err, "a repeat claim on an already-terminal row must answer by state, not error")
	require.True(t, repeat.Existing)
	require.Equal(t, "completed", repeat.Row.State)
	require.Equal(t, "ok", repeat.Row.ResultSummary.String)
}

func TestAsyncJobStore_TransitionWonThenLostThenGone(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "echo hi"})
	require.NoError(t, err)

	won, err := store.Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "call-1", State: "completed", ResultSummary: "ok", Wake: true,
	})
	require.NoError(t, err)
	require.Equal(t, TransitionWon, won.Outcome)
	require.Equal(t, "completed", won.Row.State)
	require.Equal(t, "pending", won.Row.Delivery, "a terminal transition sets delivery=pending (DUR-2)")
	require.EqualValues(t, 1, won.Row.Wake)

	// A second transition attempt on the now-terminal row must lose the CAS
	// and adopt the committed state, not error.
	lost, err := store.Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "call-1", State: "cancelled", NoticeKind: "job_kill",
	})
	require.NoError(t, err)
	require.Equal(t, TransitionLost, lost.Outcome)
	require.Equal(t, "completed", lost.Row.State, "the loser must adopt the WINNER's state, not its own")

	// A transition against a row that no longer exists reports Gone.
	gone, err := store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "no-such-call", State: "failed"})
	require.NoError(t, err)
	require.Equal(t, TransitionGone, gone.Outcome)
}

// TestAsyncJobStore_TransitionPreservesVoidDelivery pins doc sec.3.8: the
// terminal transition always preserves an existing 'void' delivery instead
// of resetting it to 'pending' -- this is the ONLY terminal-transition
// query (the non-preserving sibling was deleted in this step).
func TestAsyncJobStore_TransitionPreservesVoidDelivery(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "echo hi"})
	require.NoError(t, err)

	// Simulate a Rerun truncation having already voided this row's
	// delivery before the terminal transition lands (job_kill racing
	// history truncation, doc sec.3.8).
	_, err = store.sqlDB.ExecContext(ctx, `UPDATE async_jobs SET delivery = 'void' WHERE owner_session_id = ? AND tool_call_id = ?`, "owner-1", "call-1")
	require.NoError(t, err)

	res, err := store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "cancelled", NoticeKind: "job_kill"})
	require.NoError(t, err)
	require.Equal(t, TransitionWon, res.Outcome)
	require.Equal(t, "void", res.Row.Delivery, "delivery must stay 'void', never resurrected to 'pending'")
}

func TestAsyncJobStore_MarkAnnouncedAndDeleteUnannounced(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))
	row, err := store.q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: "owner-1", ToolCallID: "call-1"})
	require.NoError(t, err)
	require.EqualValues(t, 1, row.Announced)

	// A gone row (already deleted/never existed) is reported distinctly,
	// not as a generic I/O error.
	require.ErrorIs(t, store.MarkAnnounced(ctx, "owner-1", "no-such-call"), ErrAsyncJobGone)

	_, err = store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-2", Kind: JobKindCommand, Input: "y"})
	require.NoError(t, err)
	require.NoError(t, store.DeleteUnannounced(ctx, "owner-1", "call-2"))
	_, err = store.q.GetAsyncJob(ctx, db.GetAsyncJobParams{OwnerSessionID: "owner-1", ToolCallID: "call-2"})
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestAsyncJobStore_TransitionUnderBusyDBReturnsAnErrorNotSilentRetry pins
// the layering boundary: retry-with-backoff (doc sec.3.1, DUR-1a) lives in
// the CALLER (workLedger.transition, internal/agent), not inside the store.
// A genuinely busy DB (a second connection holding a write transaction,
// with busy_timeout=0 so SQLITE_BUSY surfaces immediately instead of
// blocking) makes Transition return an error right away.
func TestAsyncJobStore_TransitionUnderBusyDBReturnsAnErrorNotSilentRetry(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	ctx := context.Background()
	// db.Connect (busy_timeout=30000) only creates+migrates the schema here;
	// the store itself uses a SEPARATE low-busy_timeout connection below --
	// otherwise the driver would silently block for up to 30s waiting out
	// the holder instead of surfacing SQLITE_BUSY as an error quickly.
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	require.NoError(t, seedSession(ctx, db.New(conn), "owner-1"))
	require.NoError(t, db.Release(dataDir))

	path := filepath.Join(dataDir, "rush.db")
	storeConn, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(20)")
	require.NoError(t, err)
	storeConn.SetMaxOpenConns(1)
	defer storeConn.Close()
	store := NewAsyncJobStore(storeConn, dataDir, 1, "test")
	_, err = store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)
	defer func() { _ = store.Close(context.Background()) }()

	// A second, independent raw connection to the SAME file, with
	// busy_timeout(0) so contention is immediate, holding an open write
	// transaction against an unrelated table for the duration of this test.
	rawDB, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
	require.NoError(t, err)
	rawDB.SetMaxOpenConns(1)
	defer rawDB.Close()
	tx, err := rawDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO async_hosts (id, pid, label, started_at) VALUES ('busy-probe', 1, '', 0)`)
	require.NoError(t, err)

	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "completed"})
	require.Error(t, err, "a genuinely busy DB must surface as an error here, not block/retry silently")
}
