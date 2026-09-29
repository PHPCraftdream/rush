package session

// Reaction debt coverage (docs/plans/2026-09-28-async-phase4-durable-core.md
// sec.3.4, step 4): the reacted marker's one-transaction write, the debt
// predicate, the Stop-tree wake=0 pass, and the host-liveness wrapper the
// CLI loop's scope predicate needs. Real SQLite throughout -- see
// newTestStore (async_job_store_test.go).

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// failAfterUpdateMessages wraps a real message.Service so its UpdateTx
// performs the REAL write through the given tx (proving it actually ran),
// then deliberately corrupts the SAME transaction with invalid SQL. Used by
// TestMarkReactedWithMessageUpdate_TransactionIsAtomic to prove
// MarkReactedWithMessageUpdate's write is genuinely one atomic transaction,
// not just "the async_jobs write happens to run after the message write":
// if the already-executed message write is still visible after the induced
// failure, they were never really one transaction.
type failAfterUpdateMessages struct {
	message.Service
}

func (f failAfterUpdateMessages) UpdateTx(ctx context.Context, tx *sql.Tx, msg message.Message) (func(), error) {
	if _, err := f.Service.UpdateTx(ctx, tx, msg); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "this is not valid sql"); err != nil {
		return nil, err
	}
	return func() {}, nil
}

// buildTestJobNoticeParams mirrors agent_notice_pull.go's real formatter
// closely enough for this test: a plain user-role notice message.
func buildTestJobNoticeParams(row JobNoticeRow) message.CreateMessageParams {
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: "job " + row.ToolCallID + " finished: " + row.ResultContent}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
	}
}

// TestMarkReactedWithMessageUpdate_OneTransactionClearsDebt pins DUR-4's
// reacted marker: a model step with real content persists AND marks
// reacted=1 on every wake=1/reacted=0/delivery='done' row of the session, in
// ONE transaction -- proven end-to-end: Claim -> announce -> terminal
// transition (wake=1) -> a REAL pull (PullJobNotices, exactly what the
// driver's turn-start pull does) moves it to delivery='done' -- debt exists
// -- then MarkReactedWithMessageUpdate (the model step's own write) clears
// it and persists the message update.
//
// REVERT CHECK: commented out the `MarkAsyncJobsReactedForOwner` call inside
// MarkReactedWithMessageUpdate (async_job_reaction.go) -- this test's final
// `require.False(t, debt)` FAILED (debt was still true after the "reaction"
// write). Restored the call; re-ran, passed. Also independently verified
// the companion failure mode: reverting the message write instead (skip
// messages.UpdateTx) left the persisted message's parts unset, failing the
// FullText assertion -- both halves of the one-transaction claim are load-
// bearing.
func TestMarkReactedWithMessageUpdate_OneTransactionClearsDebt(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	messages := message.NewService(db.New(store.sqlDB))

	_, err := store.Claim(ctx, ClaimParams{
		Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "echo hi", ToolName: "bash",
	})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))
	_, err = store.Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "call-1", State: "completed", ResultSummary: "hi", Wake: true,
	})
	require.NoError(t, err)

	// The turn-start pull: the SAME mechanism the driver uses (agent_notice_
	// pull.go), moving the row to delivery='done' and inserting the history
	// message -- this is what makes the row's wake=1/reacted=0/delivery=done
	// shape debt (DUR-4), not the terminal transition by itself.
	pulled, err := store.PullJobNotices(ctx, messages, "owner-1", buildTestJobNoticeParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1)
	require.True(t, pulled[0].Wake)

	debt, err := store.ReactionDebtExists(ctx, "owner-1")
	require.NoError(t, err)
	require.True(t, debt, "precondition: a pulled, unreacted wake=1 row must be debt")

	// The model step's own write: a real-content assistant message, in the
	// SAME transaction as the reacted marker.
	assistant, err := messages.Create(ctx, "owner-1", message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{}})
	require.NoError(t, err)
	assistant.Parts = append(assistant.Parts, message.TextContent{Text: "done"})
	assistant.AddFinish(message.FinishReasonEndTurn, "", "")

	require.NoError(t, store.MarkReactedWithMessageUpdate(ctx, messages, "owner-1", assistant))

	debt, err = store.ReactionDebtExists(ctx, "owner-1")
	require.NoError(t, err)
	require.False(t, debt, "the reacted marker must clear the debt it just observed")

	job, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.EqualValues(t, 1, job.Reacted)

	persisted, err := messages.Get(ctx, assistant.ID)
	require.NoError(t, err)
	require.Equal(t, "done", persisted.FullText(), "the message write half of the same transaction must be durable too")
}

// TestMarkReactedWithMessageUpdate_TransactionIsAtomic proves
// MarkReactedWithMessageUpdate's write is genuinely ONE transaction, not two
// writes that merely happen to run back to back (A-b test fix: the original
// test's REVERT CHECK only proved the async_jobs write was NECESSARY, never
// that a later failure rolls the earlier write back). The message write
// (first statement) is made to actually execute via a real UpdateTx call,
// then the SAME transaction is deliberately corrupted before the async_jobs/
// session_notices writes ever run. If the two writes were not truly bound to
// one transaction, the message write would survive; atomicity requires it
// does not.
//
// REVERT CHECK: temporarily made MarkReactedWithMessageUpdate commit right
// after messages.UpdateTx (before the corrupting statement had a chance to
// run) -- this test's final `require.Equal(t, "before", ...)` FAILED (the
// message showed "corrupted-write" instead), proving the assertion actually
// distinguishes committed-early from rolled-back. Restored the real
// production code (no early commit); re-ran, passed.
func TestMarkReactedWithMessageUpdate_TransactionIsAtomic(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	real := message.NewService(db.New(store.sqlDB))

	assistant, err := real.Create(ctx, "owner-1", message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "before"}},
	})
	require.NoError(t, err)

	mutated := assistant
	mutated.Parts = []message.ContentPart{message.TextContent{Text: "corrupted-write"}}
	mutated.AddFinish(message.FinishReasonEndTurn, "", "")

	wrapped := failAfterUpdateMessages{Service: real}
	err = store.MarkReactedWithMessageUpdate(ctx, wrapped, "owner-1", mutated)
	require.Error(t, err, "the induced mid-transaction failure must surface, not be swallowed")

	persisted, err := real.Get(ctx, assistant.ID)
	require.NoError(t, err)
	require.Equal(t, "before", persisted.FullText(),
		"a write already executed inside a transaction that later fails must roll back, not partially commit")
}

// TestMarkReactedWithMessageUpdate_SessionNoticesHalf is the session_notices
// twin: a supervision-style notice (no async_jobs row) also clears via the
// same one-transaction write.
func TestMarkReactedWithMessageUpdate_SessionNoticesHalf(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-2"))
	messages := message.NewService(db.New(store.sqlDB))

	// A supervision notice voids at pull unless the scope still has a
	// running row (doc sec.3.4) -- keep one open for this test's purpose.
	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-2", ToolCallID: "still-open", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)

	require.NoError(t, store.InsertSessionNotice(ctx, "owner-2", "supervision", "check-in", true, ""))
	pulled, err := store.PullSessionNotices(ctx, messages, "owner-2", func(row SessionNoticeRow) message.CreateMessageParams {
		return message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: row.Text}}, AutoResumed: true, BackgroundJobNotice: true}
	})
	require.NoError(t, err)
	require.Len(t, pulled, 1)

	debt, err := store.ReactionDebtExists(ctx, "owner-2")
	require.NoError(t, err)
	require.True(t, debt)

	assistant, err := messages.Create(ctx, "owner-2", message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{}})
	require.NoError(t, err)
	assistant.Parts = append(assistant.Parts, message.TextContent{Text: "acknowledged"})
	assistant.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, store.MarkReactedWithMessageUpdate(ctx, messages, "owner-2", assistant))

	debt, err = store.ReactionDebtExists(ctx, "owner-2")
	require.NoError(t, err)
	require.False(t, debt)
}

// TestSetWakeZeroForOwners_OnlyTouchesPendingAndDoneRows pins the Stop-tree
// wake=0 pass (doc sec.3.4/3.8): a still-RUNNING row is untouched (its wake
// bit is meaningless until terminal), while pending/done wake=1 rows of the
// listed owners lose their wake bit -- on BOTH async_jobs and session_notices
// -- and an owner NOT in the passed list is left completely alone (negative
// control, A-b test fix: the original test asserted only the session_notices
// ROW COUNT, never its wake bit, and had no owner excluded from the call to
// prove out-of-scope owners survive).
//
// REVERT CHECK: changed the query bound to this test's call from
// []string{"owner-a", "owner-b"} (excluding "owner-c") -- owner-c's notice
// wake stayed 1 as expected (the negative control is a no-op check by
// construction), but reverting owner-b's OWN inclusion instead (dropping it
// from the call) made `require.EqualValues(t, 0, noticeB.Wake, ...)` FAIL as
// expected, proving that assertion -- previously absent -- is meaningful.
// Restored the full ["owner-a", "owner-b"] call before committing.
func TestSetWakeZeroForOwners_OnlyTouchesPendingAndDoneRows(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-a"))
	require.NoError(t, seedSession(ctx, q, "owner-b"))
	require.NoError(t, seedSession(ctx, q, "owner-c"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-a", ToolCallID: "still-running", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)

	_, err = store.Claim(ctx, ClaimParams{Owner: "owner-a", ToolCallID: "done-call", Kind: JobKindCommand, Input: "y"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-a", "done-call"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-a", ToolCallID: "done-call", State: "completed", Wake: true})
	require.NoError(t, err)

	require.NoError(t, store.InsertSessionNotice(ctx, "owner-b", "supervision", "n", true, ""))
	// owner-c is the negative control: never passed to SetWakeZeroForOwners.
	require.NoError(t, store.InsertSessionNotice(ctx, "owner-c", "supervision", "n", true, ""))

	require.NoError(t, store.SetWakeZeroForOwners(ctx, []string{"owner-a", "owner-b"}))

	stillRunning, err := store.Get(ctx, "owner-a", "still-running")
	require.NoError(t, err)
	require.Equal(t, "running", stillRunning.State)

	doneCall, err := store.Get(ctx, "owner-a", "done-call")
	require.NoError(t, err)
	require.EqualValues(t, 0, doneCall.Wake, "a pending/done row of a stopped owner must lose its wake bit")

	noticesB, err := q.ListSessionNoticesForOwner(ctx, "owner-b")
	require.NoError(t, err)
	require.Len(t, noticesB, 1)
	require.EqualValues(t, 0, noticesB[0].Wake, "the session_notices row of a stopped owner must lose its wake bit too")

	noticesC, err := q.ListSessionNoticesForOwner(ctx, "owner-c")
	require.NoError(t, err)
	require.Len(t, noticesC, 1)
	require.EqualValues(t, 1, noticesC[0].Wake, "an owner NOT passed to SetWakeZeroForOwners must be left untouched")

	debtA, err := store.ReactionDebtExists(ctx, "owner-a")
	require.NoError(t, err)
	require.False(t, debtA, "wake=0 rows are no longer debt")

	debtC, err := store.ReactionDebtExists(ctx, "owner-c")
	require.NoError(t, err)
	require.True(t, debtC, "owner-c's own debt must still be open")
}

// TestHostNotDead_SelfAliveDeadUnknown pins doc sec.3.5/3.6's three-way
// classification the CLI loop's ScopeOpen relies on: this store's OWN host
// id is never probed (trivially not-dead); a released, file-still-present
// host (the "crashed process" shape -- a row still exists so Close never
// deletes the file) is provably dead; a host id that was never registered
// at all (ENOENT) is ALSO dead, not merely unknown; and a genuinely
// indeterminate probe (A-b test fix: the original test never exercised this
// branch at all) is reported not-dead, per doc sec.3.6's "treat unknown as
// not-dead".
func TestHostNotDead_SelfAliveDeadUnknown(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)
	hostID := store.HostID()
	require.NotEmpty(t, hostID)

	require.True(t, store.HostNotDead(hostID), "a store's own host id must never be probed and is trivially not dead")

	// A directory sitting at the lock path is neither a live lock nor ENOENT
	// -- ProbeHost surfaces an error that is not a definite "dead" verdict,
	// so HostNotDead must fold that into "not dead" (doc sec.3.6), not into
	// "dead and reapable".
	unknownHostID := "host-unknown-1"
	require.NoError(t, os.MkdirAll(HostLockPath(store.dataDir, unknownHostID), 0o755))
	require.True(t, store.HostNotDead(unknownHostID), "an indeterminate probe outcome must never be treated as dead")

	require.NoError(t, store.Close(ctx))
	// Close released the OS lock; the row is still 'running' (never
	// transitioned), so the file itself was never deleted -- exactly the
	// shape a crashed process' host file leaves behind for a recoverer.
	require.False(t, store.HostNotDead(hostID), "a released, unlocked host file must be reported as dead")

	require.False(t, store.HostNotDead("host-id-never-registered"), "ENOENT counts as dead per doc sec.3.6, not merely unknown")
}

// TestSessionNoticesOwnerIndex_UsedByListForOwner is A9's fix: session_notices
// had no plain (owner) index, so ListSessionNoticesForOwner (no other
// predicate to narrow it) fell back to a full table scan. Asserted via a
// real EXPLAIN QUERY PLAN against the migrated schema -- a "SCAN" without
// "USING INDEX" means no index accelerated the lookup at all.
//
// REVERT CHECK: ran this against the schema from BEFORE migration
// 20260929000001 (index absent) -- the plan showed a bare "SCAN
// session_notices" with no "USING INDEX" clause, and this test's assertion
// FAILED as expected. Restored the migration; re-ran, passed.
func TestSessionNoticesOwnerIndex_UsedByListForOwner(t *testing.T) {
	t.Parallel()
	store, _, ctx := newTestStore(t)

	rows, err := store.sqlDB.QueryContext(ctx,
		`EXPLAIN QUERY PLAN SELECT * FROM session_notices WHERE owner = ? ORDER BY id ASC`, "owner-1")
	require.NoError(t, err)
	defer rows.Close()

	var found bool
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notUsed, &detail))
		if strings.Contains(detail, "USING INDEX idx_session_notices_owner") {
			found = true
		}
	}
	require.NoError(t, rows.Err())
	require.True(t, found, "ListSessionNoticesForOwner must use the new plain (owner) index, not a full table scan")
}

// TestHasRunningDelegationFor pins the child-session policy check (doc
// sec.3.4): true only while a RUNNING delegation row claims the child.
func TestHasRunningDelegationFor(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "parent-1"))

	running, err := store.HasRunningDelegationFor(ctx, "child-1")
	require.NoError(t, err)
	require.False(t, running, "no delegation claims child-1 yet")

	_, err = store.Claim(ctx, ClaimParams{
		Owner: "parent-1", ToolCallID: "call-1", Kind: JobKindAgent, Input: "do work", ChildSessionID: "child-1",
	})
	require.NoError(t, err)

	running, err = store.HasRunningDelegationFor(ctx, "child-1")
	require.NoError(t, err)
	require.True(t, running)

	require.NoError(t, store.MarkAnnounced(ctx, "parent-1", "call-1"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "parent-1", ToolCallID: "call-1", State: "completed", Wake: true})
	require.NoError(t, err)

	running, err = store.HasRunningDelegationFor(ctx, "child-1")
	require.NoError(t, err)
	require.False(t, running, "a resolved delegation no longer claims its child")
}

// TestSettleByFailure_ScopedIncrementAndSettle_GoLayer is the Go-orchestration
// twin of the db package's SQL-scoping tests: CaptureDebtSnapshot captures
// exactly the current debt rows, IncrementWakeAttempts/SettleReactedFailed
// act on exactly that captured snapshot (not whatever is pending later), and
// ListReactedFailedText surfaces the closed rows' text for a failed
// delegation's parent notification.
func TestSettleByFailure_ScopedIncrementAndSettle_GoLayer(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	messages := message.NewService(db.New(store.sqlDB))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "completed", ResultSummary: "boom", Wake: true})
	require.NoError(t, err)
	_, err = store.PullJobNotices(ctx, messages, "owner-1", buildTestJobNoticeParams)
	require.NoError(t, err)

	snap, err := store.CaptureDebtSnapshot(ctx, "owner-1")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"call-1"}, snap.JobIDs)

	// A row that becomes debt AFTER the snapshot was captured must not be
	// swept up by a settle scoped to the earlier snapshot.
	_, err = store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-2", Kind: JobKindCommand, Input: "y"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-2"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-2", State: "completed", ResultSummary: "late", Wake: true})
	require.NoError(t, err)
	_, err = store.PullJobNotices(ctx, messages, "owner-1", buildTestJobNoticeParams)
	require.NoError(t, err)

	for range 3 {
		require.NoError(t, store.IncrementWakeAttempts(ctx, "owner-1", snap))
	}
	attempts, err := store.MaxWakeAttempts(ctx, "owner-1", snap)
	require.NoError(t, err)
	require.EqualValues(t, 3, attempts)

	require.NoError(t, store.SettleReactedFailed(ctx, "owner-1", snap))

	failed, err := store.ListReactedFailedText(ctx, "owner-1")
	require.NoError(t, err)
	require.Len(t, failed, 1)
	require.Equal(t, "call-1", failed[0].ToolCallID)
	require.Equal(t, "boom", failed[0].Text)

	call2, err := store.Get(ctx, "owner-1", "call-2")
	require.NoError(t, err)
	require.EqualValues(t, 0, call2.Reacted, "the later row outside the captured snapshot must survive untouched")

	debt, err := store.ReactionDebtExists(ctx, "owner-1")
	require.NoError(t, err)
	require.True(t, debt, "call-2's own debt must still be open")
}

// TestSettleReactedFailedWithMarker_SettlesAndInsertsMarkerAtomically pins
// A10's fix: settling both tables and inserting the wake_failed marker are
// ONE transaction, and the marker is inserted ONLY when rows were actually
// settled.
//
// REVERT CHECK: temporarily returned settled=1 (a fabricated non-zero count)
// without ever calling InsertSessionNotice in the "settled == 0" branch's
// sibling path -- the marker-count assertion below FAILED (0 notices found,
// not 1), proving the marker-only-if-settled gating is exercised. Separately
// verified the empty-snapshot path returns (0, nil) with zero notices
// inserted, proving the guard covers both directions.
func TestSettleReactedFailedWithMarker_SettlesAndInsertsMarkerAtomically(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	messages := message.NewService(db.New(store.sqlDB))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-1", ToolCallID: "call-1", State: "completed", ResultSummary: "boom", Wake: true})
	require.NoError(t, err)
	_, err = store.PullJobNotices(ctx, messages, "owner-1", buildTestJobNoticeParams)
	require.NoError(t, err)

	snap, err := store.CaptureDebtSnapshot(ctx, "owner-1")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"call-1"}, snap.JobIDs)

	settled, err := store.SettleReactedFailedWithMarker(ctx, "owner-1", snap, "delegation failed after retries")
	require.NoError(t, err)
	require.EqualValues(t, 1, settled)

	job, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.EqualValues(t, 1, job.Reacted)
	require.EqualValues(t, 1, job.ReactedFailed)

	notices, err := store.ListSessionNotices(ctx, "owner-1")
	require.NoError(t, err)
	require.Len(t, notices, 1, "settling a non-empty snapshot must insert exactly one marker notice")
	require.Equal(t, NoticeKindWakeFailed, notices[0].Kind)
	require.Equal(t, "delegation failed after retries", notices[0].Text)

	// A snapshot that settles NOTHING (every row already resolved by the
	// time settle runs) must insert NO marker at all.
	empty := DebtSnapshot{JobIDs: []string{"call-1"}} // already reacted/reacted_failed=1 -- the guarded UPDATE affects 0 rows
	settled, err = store.SettleReactedFailedWithMarker(ctx, "owner-1", empty, "should never appear")
	require.NoError(t, err)
	require.EqualValues(t, 0, settled)

	notices, err = store.ListSessionNotices(ctx, "owner-1")
	require.NoError(t, err)
	require.Len(t, notices, 1, "settling zero rows must not insert a second marker")
}

// ============================================================
// A1: reacted_failed is scoped to the CURRENT delegation, not permanent.
// ============================================================

// TestReactedFailed_RealReactionClearsStaleFailureWithinOneDelegation pins
// A1's first scenario: two jobs inside ONE delegation (same child session,
// same running delegation row throughout) -- the first is settled by
// failure, the second is later reacted to with real content. The real
// reaction must clear the first job's stale reacted_failed flag, so a
// verdict reader (ListReactedFailedText) never resurfaces it once the
// delegation is genuinely answering again.
//
// REVERT CHECK: temporarily removed the ClearReactedFailedForOwner call from
// MarkReactedWithMessageUpdate (async_job_reaction.go). This test's
// `require.Empty(t, failedAfter, ...)` FAILED (call-1's stale entry was
// still reported). Restored the clear; re-ran, passed.
func TestReactedFailed_RealReactionClearsStaleFailureWithinOneDelegation(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "child-1"))
	messages := message.NewService(db.New(store.sqlDB))

	// Job 1: settled by failure (simulates K=3 exhausted retries).
	_, err := store.Claim(ctx, ClaimParams{Owner: "child-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "child-1", "call-1"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "child-1", ToolCallID: "call-1", State: "completed", ResultSummary: "boom", Wake: true})
	require.NoError(t, err)
	_, err = store.PullJobNotices(ctx, messages, "child-1", buildTestJobNoticeParams)
	require.NoError(t, err)
	snap, err := store.CaptureDebtSnapshot(ctx, "child-1")
	require.NoError(t, err)
	require.NoError(t, store.SettleReactedFailed(ctx, "child-1", snap))

	failedBefore, err := store.ListReactedFailedText(ctx, "child-1")
	require.NoError(t, err)
	require.Len(t, failedBefore, 1, "precondition: call-1's failure closure is visible")

	// Job 2: same delegation, a LATER job that gets a real reaction.
	_, err = store.Claim(ctx, ClaimParams{Owner: "child-1", ToolCallID: "call-2", Kind: JobKindCommand, Input: "y"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "child-1", "call-2"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "child-1", ToolCallID: "call-2", State: "completed", ResultSummary: "fine", Wake: true})
	require.NoError(t, err)
	_, err = store.PullJobNotices(ctx, messages, "child-1", buildTestJobNoticeParams)
	require.NoError(t, err)

	assistant, err := messages.Create(ctx, "child-1", message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{}})
	require.NoError(t, err)
	assistant.Parts = append(assistant.Parts, message.TextContent{Text: "done"})
	assistant.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, store.MarkReactedWithMessageUpdate(ctx, messages, "child-1", assistant))

	failedAfter, err := store.ListReactedFailedText(ctx, "child-1")
	require.NoError(t, err)
	require.Empty(t, failedAfter, "a real reaction must clear an earlier, superseded failure closure in the SAME delegation")
}

// TestReactedFailed_NewDelegationClaimClearsStaleFailureFromPriorDelegation
// pins A1's second scenario: a child C whose PREVIOUS delegation closed by
// failure (reacted_failed=1) must start a FRESH delegation with no stale
// flag -- cleared at Claim time, before the new delegation has produced a
// single reaction of its own.
//
// REVERT CHECK: temporarily removed the ClearReactedFailedForOwner calls
// from claimOnce's fresh-delegation-insert path (async_job_store.go). This
// test's `require.Empty(t, failedAfter, ...)` FAILED (the old delegation's
// entry was still reported even after the new delegation was claimed).
// Restored the clear; re-ran, passed.
func TestReactedFailed_NewDelegationClaimClearsStaleFailureFromPriorDelegation(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "parent-1"))
	require.NoError(t, seedSession(ctx, q, "child-1"))
	messages := message.NewService(db.New(store.sqlDB))

	// First delegation: parent-1 -> child-1, closed by failure.
	_, err := store.Claim(ctx, ClaimParams{
		Owner: "parent-1", ToolCallID: "deleg-1", Kind: JobKindAgent, Input: "do work", ChildSessionID: "child-1",
	})
	require.NoError(t, err)
	// child-1's OWN job inside that delegation, settled by failure.
	_, err = store.Claim(ctx, ClaimParams{Owner: "child-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "child-1", "call-1"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "child-1", ToolCallID: "call-1", State: "completed", ResultSummary: "boom", Wake: true})
	require.NoError(t, err)
	_, err = store.PullJobNotices(ctx, messages, "child-1", buildTestJobNoticeParams)
	require.NoError(t, err)
	snap, err := store.CaptureDebtSnapshot(ctx, "child-1")
	require.NoError(t, err)
	require.NoError(t, store.SettleReactedFailed(ctx, "child-1", snap))
	// Finish the first delegation so a second one can be claimed.
	require.NoError(t, store.MarkAnnounced(ctx, "parent-1", "deleg-1"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "parent-1", ToolCallID: "deleg-1", State: "failed", Wake: true})
	require.NoError(t, err)

	failedBefore, err := store.ListReactedFailedText(ctx, "child-1")
	require.NoError(t, err)
	require.Len(t, failedBefore, 1, "precondition: the first delegation's failure closure is visible")

	// A fresh, SECOND delegation to the SAME child.
	_, err = store.Claim(ctx, ClaimParams{
		Owner: "parent-1", ToolCallID: "deleg-2", Kind: JobKindAgent, Input: "do work again", ChildSessionID: "child-1",
	})
	require.NoError(t, err)

	failedAfter, err := store.ListReactedFailedText(ctx, "child-1")
	require.NoError(t, err)
	require.Empty(t, failedAfter, "a fresh delegation must not inherit the previous one's failure closure")
}
