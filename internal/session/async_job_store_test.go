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
	"github.com/PHPCraftdream/rush/internal/message"
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

// TestAsyncJobStore_ClaimReusedToolCallIDAfterDoneStartsFreshRow is B14/A14b's
// fix: a provider that reuses a tool_call_id text (numbering calls per
// response, e.g. "call_0") after the OLD row under that id already reached
// delivery='done' must be able to start immediately, with a brand new row --
// not refused as "already started earlier"/"different input" until 7-day
// retention purges the old row.
//
// REVERT CHECK: temporarily removed the `existing.State != "running" && ...`
// archive branch from claimOnce (async_job_store.go), restoring the old
// unconditional mismatch-or-idempotent logic. This test's second Claim then
// returned `*ErrAsyncJobInputMismatch` instead of a fresh running row, and
// the FAILED assertion below caught it. Restored the archive branch; re-ran,
// passed.
func TestAsyncJobStore_ClaimReusedToolCallIDAfterDoneStartsFreshRow(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call_0", Kind: JobKindCommand, Input: "first"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call_0"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call_0", State: "completed", ResultSummary: "first result", Wake: true})
	require.NoError(t, err)
	messages := message.NewService(db.New(store.sqlDB))
	pulled, err := store.PullJobNotices(ctx, messages, "owner-1", buildTestJobNoticeParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1, "precondition: the first row's delivery must reach 'done' before it becomes reusable")

	// A DIFFERENT input under the SAME literal tool_call_id text -- with the
	// old row still active this would be ErrAsyncJobInputMismatch; now it
	// must succeed as a brand new claim.
	second, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call_0", Kind: JobKindCommand, Input: "second, unrelated"})
	require.NoError(t, err, "a tool_call_id whose old row is already delivered history must be claimable again")
	require.False(t, second.Existing)
	require.Equal(t, "running", second.Row.State)
	require.Equal(t, "call_0", second.Row.ToolCallID, "the fresh claim must be addressable by the plain, reused id")

	// The OLD row must still exist (archived, not deleted) -- addressable by
	// its own notice_message_id for Rerun/readers -- just no longer under the
	// plain "call_0" key.
	all, err := q.ListAsyncJobsForOwner(ctx, "owner-1")
	require.NoError(t, err)
	require.Len(t, all, 2, "the old row must survive, archived under a different key, alongside the fresh one")
	var sawArchived bool
	for _, row := range all {
		if row.ToolCallID != "call_0" {
			sawArchived = true
			require.Equal(t, "first result", row.ResultSummary.String, "the archived row must keep its own history intact")
			require.True(t, row.NoticeMessageID.Valid, "the archived row must stay addressable by its own notice message id")
		}
	}
	require.True(t, sawArchived, "the old row must have been archived, not deleted, and not left under the reused key")
}

// TestAsyncJobStore_ClaimReusedToolCallIDStillRunningIsRefused proves the
// archive-on-reuse fix (B14) does NOT apply to a row that is merely
// delivery='void' while STILL running (Rerun can void a still-running row) --
// archiving such a row would orphan its own in-flight executor's eventual
// Transition call. The reused id must still be refused while the old row is
// live, exactly like before this fix.
func TestAsyncJobStore_ClaimReusedToolCallIDStillRunningIsRefused(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call_0", Kind: JobKindCommand, Input: "first"})
	require.NoError(t, err)
	// Void it directly at the DB level while state stays 'running' -- the
	// exact shape Rerun's VoidAsyncJobsByToolCallIDs produces for a stop that
	// raced or failed.
	_, err = q.VoidAsyncJobsByToolCallIDs(ctx, db.VoidAsyncJobsByToolCallIDsParams{
		UpdatedAt: 2, OwnerSessionID: "owner-1", ToolCallIds: []string{"call_0"},
	})
	require.NoError(t, err)
	row, err := store.Get(ctx, "owner-1", "call_0")
	require.NoError(t, err)
	require.Equal(t, "running", row.State)
	require.Equal(t, "void", row.Delivery)

	_, err = store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call_0", Kind: JobKindCommand, Input: "different input"})
	require.Error(t, err, "a still-running row (even voided) must not be archived out from under its own in-flight executor")
	var mismatch *ErrAsyncJobInputMismatch
	require.ErrorAs(t, err, &mismatch)
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

// TestAsyncJobStore_ClaimFailsClosedAgainstAClosedDB is DUR-8's genuinely-
// unavailable-but-configured-DB case (docs/async-invariants.md's own DUR-8
// row: "a broken real *sql.DB is covered indirectly ... not a dedicated
// Start-level test against a closed connection"; the review's Test fixes
// list asks for exactly that at the store level, complementing
// internal/agent's "no store wired" case which this package cannot reach).
// A *sql.DB that is CLOSED mid-process (not merely "nil store") must make
// Claim return a plain error immediately -- never panic, never silently
// succeed, never hang -- so workLedger.Start's fail-closed propagation has
// something real to propagate.
func TestAsyncJobStore_ClaimFailsClosedAgainstAClosedDB(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	ctx := context.Background()
	// db.Connect only creates+migrates the schema here (same pattern as
	// TestAsyncJobStore_TransitionUnderBusyDBReturnsAnErrorNotSilentRetry
	// above): the store's own connection below is a SEPARATE raw handle this
	// test closes directly, not the shared pooled one db.Release manages.
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	require.NoError(t, seedSession(ctx, db.New(conn), "owner-1"))
	require.NoError(t, db.Release(dataDir))

	storeConn, err := sql.Open("sqlite", filepath.Join(dataDir, "rush.db"))
	require.NoError(t, err)
	store := NewAsyncJobStore(storeConn, dataDir, 1, "test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	// One successful claim first, so the store's host identity is already
	// registered -- isolating THIS test's failure to the closed-DB claim
	// itself, not host registration.
	_, err = store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)

	require.NoError(t, storeConn.Close())

	_, err = store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-2", Kind: JobKindCommand, Input: "y"})
	require.Error(t, err, "Claim against a closed *sql.DB must fail closed with a plain error, never panic or hang")
}

// TestAsyncJobStore_SetReadConn_RoutesReadsToTheGivenConnection is A8/C9's
// fix: the cross-process reader methods (LiveJobs/JobsInTree/
// ReactionDebtExists/ListAsyncJobsForOwner) must actually run on the wired
// read-pool connection, not silently keep using the writer.
//
// REVERT CHECK: temporarily made readQuerier always return s.q (ignoring
// s.readQ). This test's final `require.Error` FAILED (the read succeeded
// against the writer, seeing the row that only exists there) -- proving the
// assertion actually distinguishes "routed to the given connection" from
// "silently used the writer". Restored readQuerier's real body; re-ran,
// passed.
func TestAsyncJobStore_SetReadConn_RoutesReadsToTheGivenConnection(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)

	rows, err := store.ListAsyncJobsForOwner(ctx, "owner-1")
	require.NoError(t, err)
	require.Len(t, rows, 1, "sanity: with no read pool wired, the reader falls back to the writer")

	// A DIFFERENT, empty on-disk DB with none of async_jobs' schema -- if
	// SetReadConn genuinely routes reads there, the SAME query must now
	// fail (proving it did not silently keep using the writer).
	otherDir := t.TempDir()
	other, err := sql.Open("sqlite", filepath.Join(otherDir, "other.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	store.SetReadConn(other)

	_, err = store.ListAsyncJobsForOwner(ctx, "owner-1")
	require.Error(t, err, "SetReadConn must actually route reader queries to the given connection")
}
