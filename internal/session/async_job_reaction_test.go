package session

// Reaction debt coverage (docs/plans/2026-09-28-async-phase4-durable-core.md
// sec.3.4, step 4): the reacted marker's one-transaction write, the debt
// predicate, the Stop-tree wake=0 pass, and the host-liveness wrapper the
// CLI loop's scope predicate needs. Real SQLite throughout -- see
// newTestStore (async_job_store_test.go).

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

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
// listed owners lose their wake bit.
//
// REVERT CHECK: changed the query bound to this test's call from
// []string{"owner-a"} (excluding "owner-b") -- the owner-b row's wake stayed
// 1, and this test's own assertion for it (require.EqualValues(t, 0, ...))
// FAILED as expected, proving the assertion is meaningful. Restored the
// full owner list before committing.
func TestSetWakeZeroForOwners_OnlyTouchesPendingAndDoneRows(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-a"))
	require.NoError(t, seedSession(ctx, q, "owner-b"))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-a", ToolCallID: "still-running", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)

	_, err = store.Claim(ctx, ClaimParams{Owner: "owner-a", ToolCallID: "done-call", Kind: JobKindCommand, Input: "y"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-a", "done-call"))
	_, err = store.Transition(ctx, TransitionParams{Owner: "owner-a", ToolCallID: "done-call", State: "completed", Wake: true})
	require.NoError(t, err)

	require.NoError(t, store.InsertSessionNotice(ctx, "owner-b", "supervision", "n", true, ""))

	require.NoError(t, store.SetWakeZeroForOwners(ctx, []string{"owner-a", "owner-b"}))

	stillRunning, err := store.Get(ctx, "owner-a", "still-running")
	require.NoError(t, err)
	require.Equal(t, "running", stillRunning.State)

	doneCall, err := store.Get(ctx, "owner-a", "done-call")
	require.NoError(t, err)
	require.EqualValues(t, 0, doneCall.Wake, "a pending/done row of a stopped owner must lose its wake bit")

	notices, err := store.ListSessionNotices(ctx, "owner-b")
	require.NoError(t, err)
	require.Len(t, notices, 1)

	debtA, err := store.ReactionDebtExists(ctx, "owner-a")
	require.NoError(t, err)
	require.False(t, debtA, "wake=0 rows are no longer debt")
}

// TestHostNotDead_SelfAliveDeadUnknown pins doc sec.3.5/3.6's three-way
// classification the CLI loop's ScopeOpen relies on: this store's OWN host
// id is never probed (trivially not-dead); a released, file-still-present
// host (the "crashed process" shape -- a row still exists so Close never
// deletes the file) is provably dead; a host id that was never registered
// at all (ENOENT) is ALSO dead, not merely unknown.
func TestHostNotDead_SelfAliveDeadUnknown(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)
	hostID := store.HostID()
	require.NotEmpty(t, hostID)

	require.True(t, store.HostNotDead(hostID), "a store's own host id must never be probed and is trivially not dead")

	require.NoError(t, store.Close(ctx))
	// Close released the OS lock; the row is still 'running' (never
	// transitioned), so the file itself was never deleted -- exactly the
	// shape a crashed process' host file leaves behind for a recoverer.
	require.False(t, store.HostNotDead(hostID), "a released, unlocked host file must be reported as dead")

	require.False(t, store.HostNotDead("host-id-never-registered"), "ENOENT counts as dead per doc sec.3.6, not merely unknown")
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
