// TruncateForRerun coverage (docs/reviews/2026-09-29-async-phase4-round1-
// rerun-design.md, Problem 1): the ONE writer transaction a Rerun runs --
// atomic with the message deletes, reconciled only against the rows it
// actually deleted, void keyed by the announcing message id (not by a
// provider-reusable tool_call_id), void beating re-pend. Real SQLite.
package session

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

const rerunOwner = "owner-1"

// rerunFx is one owner session with a target user message and helpers that
// append history rows in order.
type rerunFx struct {
	t        *testing.T
	store    *AsyncJobStore
	q        *db.Queries
	ctx      context.Context
	messages message.Service
	target   message.Message
}

func newRerunFx(t *testing.T) *rerunFx {
	t.Helper()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, rerunOwner))
	f := &rerunFx{t: t, store: store, q: q, ctx: ctx, messages: message.NewService(q)}
	f.target = f.add(message.User, message.TextContent{Text: "rerun me"})
	return f
}

func (f *rerunFx) add(role message.MessageRole, parts ...message.ContentPart) message.Message {
	f.t.Helper()
	m, err := f.messages.Create(f.ctx, rerunOwner, message.CreateMessageParams{Role: role, Parts: parts})
	require.NoError(f.t, err)
	return m
}

// assistantCall appends an assistant message carrying one tool call.
func (f *rerunFx) assistantCall(callID string) message.Message {
	return f.add(message.Assistant, message.ToolCall{ID: callID, Name: "bash", Input: "{}", Finished: true})
}

// announced claims callID as an async job and acknowledges it through the
// ack gate's fused transaction; returns the "started" tool-result message.
func (f *rerunFx) announced(callID string) message.Message {
	f.t.Helper()
	_, err := f.store.Claim(f.ctx, ClaimParams{Owner: rerunOwner, ToolCallID: callID, Kind: JobKindCommand, Input: "in-" + callID, ToolName: "bash"})
	require.NoError(f.t, err)
	msg, err := f.store.AnnounceStarted(f.ctx, f.messages, rerunOwner, callID, message.CreateMessageParams{
		Role:  message.Tool,
		Parts: []message.ContentPart{message.ToolResult{ToolCallID: callID, Name: "bash", Content: "started"}},
	})
	require.NoError(f.t, err)
	return msg
}

// complete finishes callID (wake=true) and pulls its notice into history;
// returns the notice message.
func (f *rerunFx) complete(callID string) message.Message {
	f.t.Helper()
	_, err := f.store.Transition(f.ctx, TransitionParams{Owner: rerunOwner, ToolCallID: callID, State: "completed", Wake: true})
	require.NoError(f.t, err)
	pulled, err := f.store.PullJobNotices(f.ctx, f.messages, rerunOwner, jobNoticeParams)
	require.NoError(f.t, err)
	require.Len(f.t, pulled, 1)
	return pulled[0].Message
}

func (f *rerunFx) truncate(tailIDs ...string) (RerunTruncation, error) {
	f.t.Helper()
	return TruncateForRerun(f.ctx, f.store.sqlDB, f.messages, RerunTruncateParams{Owner: rerunOwner, TargetID: f.target.ID, TailIDs: tailIDs})
}

func (f *rerunFx) job(callID string) db.AsyncJob {
	f.t.Helper()
	row, err := f.store.Get(f.ctx, rerunOwner, callID)
	require.NoError(f.t, err)
	return row
}

func (f *rerunFx) exists(id string) bool {
	_, err := f.messages.Get(f.ctx, id)
	return err == nil
}

func voidedIDs(v []VoidedAsyncJob) []string {
	out := make([]string, 0, len(v))
	for _, j := range v {
		out = append(out, j.ToolCallID)
	}
	return out
}

func setRerunSeam(t *testing.T, fn func(step string) error) {
	t.Helper()
	rerunTruncateStepSeam = fn
	t.Cleanup(func() { rerunTruncateStepSeam = nil })
}

// TestTruncateForRerun_InjectedFailureBeforeCommit_ChangesNothing: a failure
// anywhere inside the transaction rolls back messages AND ledger together
// and publishes nothing. Not parallel: it owns the package-level seam.
//
// Revert-check: run DeleteTx in its own transaction (commit it before the
// reconciliation) -- the target/tail are gone after the injected failure.
func TestTruncateForRerun_InjectedFailureBeforeCommit_ChangesNothing(t *testing.T) {
	f := newRerunFx(t)
	asst := f.assistantCall("call-1")
	started := f.announced("call-1")
	notice := f.complete("call-1")
	sub := f.messages.Subscribe(f.ctx)

	setRerunSeam(t, func(step string) error {
		if step == "reconciled" {
			return errors.New("injected failure before commit")
		}
		return nil
	})
	_, err := f.truncate(asst.ID, started.ID, notice.ID)
	require.ErrorContains(t, err, "injected failure before commit")

	for _, id := range []string{f.target.ID, asst.ID, started.ID, notice.ID} {
		require.True(t, f.exists(id), "message %s must survive a rolled-back rerun", id)
	}
	row := f.job("call-1")
	require.Equal(t, "done", row.Delivery, "the delivered notice must not be re-pended by a rolled-back rerun")
	require.Equal(t, started.ID, row.AnnounceMessageID.String)
	select {
	case ev := <-sub:
		t.Fatalf("a rolled-back rerun published %v", ev)
	default:
	}
	_, gen, err := f.messages.ListWithWatermark(f.ctx, rerunOwner)
	require.NoError(t, err)
	require.EqualValues(t, 0, gen, "a rolled-back rerun must not bump the delete generation")
}

// TestTruncateForRerun_PublishesDeletedEventsAfterCommit: a committed rerun
// publishes one DeletedEvent per deleted row (target included) and bumps the
// delete generation; the events describe rows that are really gone.
func TestTruncateForRerun_PublishesDeletedEventsAfterCommit(t *testing.T) {
	t.Parallel()
	f := newRerunFx(t)
	asst := f.assistantCall("call-1")
	started := f.announced("call-1")
	sub := f.messages.Subscribe(f.ctx)

	res, err := f.truncate(asst.ID, started.ID)
	require.NoError(t, err)
	require.Len(t, res.Deleted, 3)
	seen := map[string]bool{}
	for range res.Deleted {
		ev := <-sub
		require.Equal(t, pubsub.DeletedEvent, ev.Type)
		require.False(t, f.exists(ev.Payload.ID), "a DeletedEvent must only be published for a row that is gone")
		seen[ev.Payload.ID] = true
	}
	require.Len(t, seen, 3)
	_, gen, err := f.messages.ListWithWatermark(f.ctx, rerunOwner)
	require.NoError(t, err)
	require.EqualValues(t, 3, gen)
}

// TestTruncateForRerun_TargetGone_ChangesNothing: the target was deleted by
// someone else (e.g. a concurrent rerun): the truncation reports it and
// changes NOTHING -- the tail and its jobs stay.
//
// Revert-check: drop the target-present check.
func TestTruncateForRerun_TargetGone_ChangesNothing(t *testing.T) {
	t.Parallel()
	f := newRerunFx(t)
	asst := f.assistantCall("call-1")
	started := f.announced("call-1")
	require.NoError(t, f.messages.Delete(f.ctx, f.target.ID))

	_, err := f.truncate(asst.ID, started.ID)
	require.ErrorIs(t, err, ErrRerunTargetGone)
	require.True(t, f.exists(asst.ID))
	require.True(t, f.exists(started.ID))
	require.Equal(t, "none", f.job("call-1").Delivery, "a refused rerun must not void anything")
}

// TestTruncateForRerun_SetsFromActuallyDeletedRows: the reconciliation sets
// come from the rows the delete statement ACTUALLY removed. A tail notice
// message someone else already deleted is not this rerun's business: its row
// stays 'done' (plain-delete semantics), while a notice T did delete is
// re-pended.
//
// Revert-check: build the id sets from p.TailIDs instead of the returned rows.
func TestTruncateForRerun_SetsFromActuallyDeletedRows(t *testing.T) {
	t.Parallel()
	f := newRerunFx(t)
	f.assistantCall("call-a")
	f.announced("call-a")
	noticeA := f.complete("call-a")
	f.assistantCall("call-b")
	f.announced("call-b")
	noticeB := f.complete("call-b")
	require.NoError(t, f.messages.Delete(f.ctx, noticeA.ID)) // gone before the rerun

	res, err := f.truncate(noticeA.ID, noticeB.ID)
	require.NoError(t, err)
	require.Len(t, res.Deleted, 2, "target + noticeB")
	require.Equal(t, "done", f.job("call-a").Delivery, "a row whose notice someone else deleted is not re-pended by this rerun")
	require.Equal(t, "pending", f.job("call-b").Delivery, "a row whose notice T deleted is re-pended")
}

// TestTruncateForRerun_VoidByAnnounceMessage_ToolCallIDReuse: voiding keys on
// the announcing message, so a provider that reuses "call_0" per response
// can neither void a KEPT job (case a) nor miss an ARCHIVED one (case b).
//
// Revert-check: void by tool_call_id only (drop the announce arm and the
// `announce_message_id IS NULL` restriction).
func TestTruncateForRerun_VoidByAnnounceMessage_ToolCallIDReuse(t *testing.T) {
	t.Parallel()

	t.Run("kept_running_job_survives_reused_id_in_tail", func(t *testing.T) {
		t.Parallel()
		f := newRerunFx(t)
		f.assistantCall("call_0") // kept history: async call_0, still running
		f.announced("call_0")
		// New target after the kept history; the tail reuses call_0 for a
		// plain (non-async) tool call.
		f.target = f.add(message.User, message.TextContent{Text: "second prompt"})
		tailCall := f.assistantCall("call_0")
		tailResult := f.add(message.Tool, message.ToolResult{ToolCallID: "call_0", Name: "view", Content: "file"})

		res, err := f.truncate(tailCall.ID, tailResult.ID)
		require.NoError(t, err)
		require.Empty(t, res.Voided, "the kept job must not be voided or stopped by a reused tool_call_id")
		row := f.job("call_0")
		require.Equal(t, "none", row.Delivery)
		require.Equal(t, "running", row.State)
	})

	t.Run("archived_job_in_tail_is_voided", func(t *testing.T) {
		t.Parallel()
		f := newRerunFx(t)
		var tail []string
		tail = append(tail, f.assistantCall("call_0").ID)
		tail = append(tail, f.announced("call_0").ID)
		tail = append(tail, f.complete("call_0").ID) // its notice is in the tail too
		tail = append(tail, f.assistantCall("call_0").ID)
		// Reusing the id archives the first (history) row.
		tail = append(tail, f.announced("call_0").ID)

		res, err := f.truncate(tail...)
		require.NoError(t, err)
		require.Len(t, res.Voided, 2, "both the archived row and the live row are voided: %v", voidedIDs(res.Voided))
		rows, err := f.store.ListAsyncJobsForOwner(f.ctx, rerunOwner)
		require.NoError(t, err)
		require.Len(t, rows, 2)
		for _, r := range rows {
			require.Equal(t, "void", r.Delivery, "row %s must be void", r.ToolCallID)
		}
	})
}

// TestTruncateForRerun_LegacyRowWithoutAnnounceIDVoidedByToolCallID: a row
// announced before the announce_message_id migration (NULL) still voids via
// the tool_call_id arm.
//
// Revert-check: drop the legacy arm.
func TestTruncateForRerun_LegacyRowWithoutAnnounceIDVoidedByToolCallID(t *testing.T) {
	t.Parallel()
	f := newRerunFx(t)
	asst := f.assistantCall("call-1")
	_, err := f.store.Claim(f.ctx, ClaimParams{Owner: rerunOwner, ToolCallID: "call-1", Kind: JobKindCommand, Input: "x"})
	require.NoError(t, err)
	require.NoError(t, f.store.MarkAnnounced(f.ctx, rerunOwner, "call-1")) // plain path: announce_message_id stays NULL

	res, err := f.truncate(asst.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"call-1"}, voidedIDs(res.Voided))
	require.Equal(t, "void", f.job("call-1").Delivery)
}

// TestTruncateForRerun_RependsNoticesVoidsWakeFailed: delivered notices whose
// message was deleted are re-pended; a wake_failed marker describes the
// deleted branch and is voided instead; a marker outside the tail stays done.
//
// Revert-check: drop `kind <> 'wake_failed'` from RependSessionNoticesByMessageIDs.
func TestTruncateForRerun_RependsNoticesVoidsWakeFailed(t *testing.T) {
	t.Parallel()
	f := newRerunFx(t)
	deliver := func(kind, text string) (int64, message.Message) {
		t.Helper()
		require.NoError(t, f.store.InsertSessionNotice(f.ctx, rerunOwner, kind, text, true, ""))
		pulled, err := f.store.PullSessionNotices(f.ctx, f.messages, rerunOwner, sessionNoticeParams)
		require.NoError(t, err)
		require.Len(t, pulled, 1)
		notices, err := f.store.ListSessionNotices(f.ctx, rerunOwner)
		require.NoError(t, err)
		return notices[len(notices)-1].ID, pulled[0].Message
	}
	bgID, bgMsg := deliver(NoticeKindBGShellDone, "shell done")
	wfID, wfMsg := deliver(NoticeKindWakeFailed, "wake failed in the deleted branch")
	keptID, _ := deliver(NoticeKindWakeFailed, "kept marker") // not passed to truncate: its message stays
	f.assistantCall("call-1")
	f.announced("call-1")
	jobNotice := f.complete("call-1")

	_, err := f.truncate(bgMsg.ID, wfMsg.ID, jobNotice.ID)
	require.NoError(t, err)

	get := func(id int64) db.SessionNotice {
		n, err := f.q.GetSessionNotice(f.ctx, id)
		require.NoError(t, err)
		return n
	}
	require.Equal(t, "pending", get(bgID).Delivery, "a delivered notice of the deleted tail is re-pended")
	require.Equal(t, "void", get(wfID).Delivery, "a wake_failed marker of the deleted tail is voided, not re-pended")
	require.Equal(t, "done", get(keptID).Delivery, "a marker whose message is kept stays delivered")
	require.Equal(t, "pending", f.job("call-1").Delivery)
}

// TestTruncateForRerun_VoidBeatsRepend: a row whose announce message AND own
// notice are both deleted ends void, never re-pended.
//
// Revert-check: drop the announce-message void arm (the row then ends pending).
// Note the A4 `delivery = 'done'` guard makes the order of voids vs re-pends
// irrelevant for a row void wins on: swapping the order alone is not a revert.
func TestTruncateForRerun_VoidBeatsRepend(t *testing.T) {
	t.Parallel()
	f := newRerunFx(t)
	asst := f.assistantCall("call-1")
	started := f.announced("call-1")
	notice := f.complete("call-1")

	_, err := f.truncate(asst.ID, started.ID, notice.ID)
	require.NoError(t, err)
	require.Equal(t, "void", f.job("call-1").Delivery)
}

// TestTruncateForRerun_RependNeverResurrectsAlreadyVoidRow pins A4: void is
// terminal for the re-pend query too. The row's notice is in the tail but an
// earlier pass already voided it (its announce message is kept): a pass
// touching only the notice must leave it void.
//
// Revert-check: drop the `delivery = 'done'` guard from
// RependAsyncJobsByNoticeMessageIDs.
func TestTruncateForRerun_RependNeverResurrectsAlreadyVoidRow(t *testing.T) {
	t.Parallel()
	f := newRerunFx(t)
	f.assistantCall("call-1")
	started := f.announced("call-1")
	notice := f.complete("call-1")
	_, err := f.q.VoidAsyncJobsByAnnounceMessageIDs(f.ctx, db.VoidAsyncJobsByAnnounceMessageIDsParams{
		UpdatedAt: 4, Owner: rerunOwner, MessageIds: nullStrings([]string{started.ID}),
	})
	require.NoError(t, err)
	require.Equal(t, "void", f.job("call-1").Delivery, "precondition")

	_, err = f.truncate(notice.ID)
	require.NoError(t, err)
	require.Equal(t, "void", f.job("call-1").Delivery, "void is terminal -- a re-pend must never resurrect it")
}

// TestTruncateForRerun_UnrelatedRowsUntouched: a row with no connection to
// the deleted rows keeps its state.
func TestTruncateForRerun_UnrelatedRowsUntouched(t *testing.T) {
	t.Parallel()
	f := newRerunFx(t)
	f.assistantCall("call-kept")
	f.announced("call-kept")
	_, err := f.store.Transition(f.ctx, TransitionParams{Owner: rerunOwner, ToolCallID: "call-kept", State: "completed", Wake: true})
	require.NoError(t, err) // pending, announce message kept
	asst := f.assistantCall("call-tail")
	started := f.announced("call-tail")

	res, err := f.truncate(asst.ID, started.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"call-tail"}, voidedIDs(res.Voided))
	require.Equal(t, "pending", f.job("call-kept").Delivery, "an unrelated row must be untouched")
}

// TestTruncateForRerun_LateTransitionKeepsVoid: a still-running row of the
// tail is voided by the truncation (state untouched -- stopping it is the
// caller's job) and a late terminal transition preserves the void.
//
// Revert-check: drop the announce-message void arm.
func TestTruncateForRerun_LateTransitionKeepsVoid(t *testing.T) {
	t.Parallel()
	f := newRerunFx(t)
	asst := f.assistantCall("call-1")
	started := f.announced("call-1")

	res, err := f.truncate(asst.ID, started.ID)
	require.NoError(t, err)
	require.Len(t, res.Voided, 1)
	require.Equal(t, "running", res.Voided[0].State)
	require.Equal(t, f.store.HostID(), res.Voided[0].HostID)
	row := f.job("call-1")
	require.Equal(t, "running", row.State, "TruncateForRerun must not itself stop the row")
	require.Equal(t, "void", row.Delivery)

	won, err := f.store.Transition(f.ctx, TransitionParams{Owner: rerunOwner, ToolCallID: "call-1", State: "cancelled", NoticeKind: "job_kill", Wake: false})
	require.NoError(t, err)
	require.Equal(t, TransitionWon, won.Outcome)
	require.Equal(t, "void", won.Row.Delivery, "a late terminal transition must keep void")
}

// TestTruncateForRerun_VoidedCarriesDelegationChild: the Voided set names the
// delegation's child session so the caller can stop its tree.
func TestTruncateForRerun_VoidedCarriesDelegationChild(t *testing.T) {
	t.Parallel()
	f := newRerunFx(t)
	asst := f.assistantCall("deleg-1")
	_, err := f.store.Claim(f.ctx, ClaimParams{Owner: rerunOwner, ToolCallID: "deleg-1", Kind: JobKindAgent, Input: "work", ChildSessionID: "child-1", ToolName: "agent"})
	require.NoError(t, err)
	started, err := f.store.AnnounceStarted(f.ctx, f.messages, rerunOwner, "deleg-1", message.CreateMessageParams{
		Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "deleg-1", Name: "agent", Content: "started"}},
	})
	require.NoError(t, err)

	res, err := f.truncate(asst.ID, started.ID)
	require.NoError(t, err)
	require.Len(t, res.Voided, 1)
	require.Equal(t, "child-1", res.Voided[0].ChildSessionID)
}

// TestTruncateForRerun_ChunksLargeTail: the IN-lists are chunked, so a tail
// far beyond one chunk is fully deleted and a job announced at the very end
// (past the first chunk) is still voided.
func TestTruncateForRerun_ChunksLargeTail(t *testing.T) {
	t.Parallel()
	f := newRerunFx(t)
	var tail []string
	for i := 0; i < rerunChunk+120; i++ {
		tail = append(tail, f.add(message.User, message.TextContent{Text: fmt.Sprintf("m%d", i)}).ID)
	}
	tail = append(tail, f.assistantCall("call-last").ID)
	tail = append(tail, f.announced("call-last").ID)

	res, err := f.truncate(tail...)
	require.NoError(t, err)
	require.Len(t, res.Deleted, len(tail)+1)
	require.Equal(t, []string{"call-last"}, voidedIDs(res.Voided))
	require.Equal(t, "void", f.job("call-last").Delivery)
	left, err := f.messages.List(f.ctx, rerunOwner)
	require.NoError(t, err)
	require.Empty(t, left)
}

// TestAnnounceStarted_RecordsAnnounceMessageID: the ack gate's fused
// transaction names the announcing message on the row.
//
// Revert-check: drop the SetAsyncJobAnnounceMessageID call from AnnounceStarted.
func TestAnnounceStarted_RecordsAnnounceMessageID(t *testing.T) {
	t.Parallel()
	f := newRerunFx(t)
	started := f.announced("call-1")
	row := f.job("call-1")
	require.EqualValues(t, 1, row.Announced)
	require.True(t, row.AnnounceMessageID.Valid)
	require.Equal(t, started.ID, row.AnnounceMessageID.String)
}

// TestPull_WakeOnlyNoticeOfVoidedRunningJob_IsVoided: a wake_only check-in of
// a job a Rerun voided while its executor may still run (foreign host, failed
// stop) is voided at pull time instead of delivered into the new branch.
//
// Revert-check: drop `|| job.Delivery == "void"` from sessionNoticeVoidCondition.
func TestPull_WakeOnlyNoticeOfVoidedRunningJob_IsVoided(t *testing.T) {
	t.Parallel()
	f := newRerunFx(t)
	asst := f.assistantCall("call-1")
	started := f.announced("call-1")
	require.NoError(t, f.store.InsertSessionNotice(f.ctx, rerunOwner, NoticeKindWakeOnly, "still running, please wait", true, "call-1"))

	_, err := f.truncate(asst.ID, started.ID)
	require.NoError(t, err)
	require.Equal(t, "running", f.job("call-1").State, "precondition: the voided job's executor is still running")

	pulled, err := f.store.PullSessionNotices(f.ctx, f.messages, rerunOwner, sessionNoticeParams)
	require.NoError(t, err)
	require.Empty(t, pulled, "a wake_only notice of a voided job must not be delivered")
	notices, err := f.store.ListSessionNotices(f.ctx, rerunOwner)
	require.NoError(t, err)
	require.Len(t, notices, 1)
	n, err := f.q.GetSessionNotice(f.ctx, notices[0].ID)
	require.NoError(t, err)
	require.Equal(t, "void", n.Delivery)
}
