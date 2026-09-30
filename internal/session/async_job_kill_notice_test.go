// A3 (docs/reviews/2026-09-29-async-phase4-round1.md): "delivery='done' =>
// the row names the message that carries its result." job_kill's own
// Transition call (causeJobKill, internal/agent/work_ledger_transition.go)
// sets delivery='done'/reacted=1 directly, bypassing the ordinary pull --
// the only other writer of notice_message_id. Without AnnounceJobKillResult
// fusing it in, that row would satisfy delivery='done' with no
// notice_message_id at all, and a Rerun past job_kill's own tool-result
// message could never find it to re-pend (RependAsyncJobsByNoticeMessageIDs
// is matched by notice_message_id).
package session

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestAnnounceJobKillResult_NamesTheRowAndSurvivesRerunRepend is the A3 law,
// end to end: job_kill's Transition call commits delivery='done', its own
// tool-result message is announced via AnnounceJobKillResult, the row names
// that message, and a Rerun truncating past it correctly re-pends the row
// for the new branch.
//
// Revert-check performed: called SetAsyncJobNoticeMessageID (unguarded, no
// AnnounceJobKillResult) directly instead, mimicking the pre-A3 state (row
// stays delivery='done' with notice_message_id NULL) -- TruncateForRerun's own
// repend query then found NOTHING to re-pend (delivery stayed 'done'). Using
// AnnounceJobKillResult as this test does, the repend fires; reverted the
// production code's fix by removing the SetAsyncJobNoticeMessageIDIfDone call
// from AnnounceJobKillResult -- this test FAILED (row.NoticeMessageID.Valid
// was false, RerunTruncate found nothing). Restored; re-ran, passed.
func TestAnnounceJobKillResult_NamesTheRowAndSurvivesRerunRepend(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	messages := message.NewService(db.New(store.sqlDB))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))

	// job_kill's own Transition call (causeJobKill, doc sec.3.2): delivery=
	// 'done'/reacted=1 directly, never 'pending'.
	transRes, err := store.Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "call-1", State: "cancelled", NoticeKind: "job_kill",
		ResultSummary: "partial output before the stop", Wake: false,
		Delivery: "done", Reacted: true,
	})
	require.NoError(t, err)
	require.Equal(t, "done", transRes.Row.Delivery)
	require.False(t, transRes.Row.NoticeMessageID.Valid, "no writer has recorded a notice message yet")

	// A3: job_kill's own tool-result message fuses notice_message_id onto
	// the SAME row, in one transaction.
	killMsg, err := store.AnnounceJobKillResult(ctx, messages, "owner-1", "call-1", message.CreateMessageParams{
		Role:  message.Tool,
		Parts: []message.ContentPart{message.TextContent{Text: "Async job call-1 (bash) was stopped (job_kill)."}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, killMsg.ID)

	row, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.True(t, row.NoticeMessageID.Valid, "A3's law: a delivery='done' row must name the message carrying its result")
	require.Equal(t, killMsg.ID, row.NoticeMessageID.String)

	// Rerun truncates PAST job_kill's own tool-result message.
	target, err := messages.Create(ctx, "owner-1", message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)
	_, err = TruncateForRerun(ctx, store.sqlDB, messages, RerunTruncateParams{
		Owner: "owner-1", TargetID: target.ID, TailIDs: []string{killMsg.ID},
	})
	require.NoError(t, err)

	repent, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "pending", repent.Delivery, "Rerun must be able to re-pend a job_kill'd row once its own notice message is deleted")
	require.EqualValues(t, 0, repent.Reacted)
}

// TestAnnounceJobKillResult_LostRaceStillPersistsMessageWithoutFusing pins
// the "0 rows affected is not an error" half: if this job_kill call's OWN
// transition did NOT win delivery='done' (some other cause already committed
// first, leaving the row 'pending' for the ordinary pull instead), the
// tool-result message for job_kill's OWN call must still be persisted --
// every tool_use needs a tool_result -- but notice_message_id must be left
// alone (SetAsyncJobNoticeMessageIDIfDone's guard), never pointing at a
// message that is not actually this row's own pulled notice.
func TestAnnounceJobKillResult_LostRaceStillPersistsMessageWithoutFusing(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	messages := message.NewService(db.New(store.sqlDB))

	_, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))

	// A DIFFERENT cause (natural finish) won first: delivery stays 'pending'.
	_, err = store.Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "call-1", State: "completed",
		ResultSummary: "finished on its own", Wake: true,
	})
	require.NoError(t, err)

	msg, err := store.AnnounceJobKillResult(ctx, messages, "owner-1", "call-1", message.CreateMessageParams{
		Role:  message.Tool,
		Parts: []message.ContentPart{message.TextContent{Text: "Async job call-1 (bash) finished."}},
	})
	require.NoError(t, err, "the message must still be persisted even though this job_kill call did not win")
	require.NotEmpty(t, msg.ID)

	row, err := store.Get(ctx, "owner-1", "call-1")
	require.NoError(t, err)
	require.Equal(t, "pending", row.Delivery, "the row belongs to the cause that actually won -- untouched by the losing job_kill call")
	require.False(t, row.NoticeMessageID.Valid, "must not fuse notice_message_id onto a row this call did not actually deliver")
}

// TestRependJobKillRowWithoutNotice is R2A-8's live-process half: job_kill's
// transition committed done/reacted=1 but the fused result write never
// happened (an error result, a cancelled context, a failed transaction), so
// the row names no message. RependJobKillRowWithoutNotice puts exactly that
// row back to a plain pending, wake=0 notice -- and touches nothing else: not a
// row that already names its message, not another claim under the same
// tool_call_id, not a row of another cause.
//
// REVERT CHECK: the method's UPDATE removed (returning false, nil) -- the row
// stayed 'done' and `require.True(t, repended)` failed. Dropping the
// `notice_message_id IS NULL` or `claim_id` guard from the query makes the
// "names its message"/"other claim" sub-checks fail instead.
func TestRependJobKillRowWithoutNotice(t *testing.T) {
	t.Parallel()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	messages := message.NewService(db.New(store.sqlDB))

	kill := func(id string) db.AsyncJob {
		t.Helper()
		claimed, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: id, Kind: JobKindCommand, Input: id, ToolName: "bash"})
		require.NoError(t, err)
		require.NoError(t, store.MarkAnnounced(ctx, "owner-1", id))
		res, err := store.Transition(ctx, TransitionParams{
			Owner: "owner-1", ToolCallID: id, ClaimID: claimed.Row.ClaimID, State: "cancelled", NoticeKind: "job_kill",
			ResultSummary: "output of " + id, Delivery: "done", Reacted: true,
		})
		require.NoError(t, err)
		require.Equal(t, TransitionWon, res.Outcome)
		return res.Row
	}

	lost := kill("call-lost")
	repended, err := store.RependJobKillRowWithoutNotice(ctx, "owner-1", "call-lost", "some-other-claim")
	require.NoError(t, err)
	require.False(t, repended, "another claim's id must not re-pend this row")

	repended, err = store.RependJobKillRowWithoutNotice(ctx, "owner-1", "call-lost", lost.ClaimID)
	require.NoError(t, err)
	require.True(t, repended)
	row, err := store.Get(ctx, "owner-1", "call-lost")
	require.NoError(t, err)
	require.Equal(t, "pending", row.Delivery)
	require.EqualValues(t, 0, row.Reacted)
	require.EqualValues(t, 0, row.Wake)

	repended, err = store.RependJobKillRowWithoutNotice(ctx, "owner-1", "call-lost", lost.ClaimID)
	require.NoError(t, err)
	require.False(t, repended, "the repair is idempotent: a pending row is not touched again")

	pulled, err := store.PullJobNotices(ctx, messages, "owner-1", buildTestJobNoticeParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1)
	require.Contains(t, pulled[0].Message.FullText(), "output of call-lost")
	require.False(t, pulled[0].Wake, "the repaired job_kill notice never wakes")

	// A row that already names its result message is left alone.
	named := kill("call-named")
	_, err = store.AnnounceJobKillResult(ctx, messages, "owner-1", "call-named", message.CreateMessageParams{
		Role: message.Tool, Parts: []message.ContentPart{message.TextContent{Text: "stopped"}},
	})
	require.NoError(t, err)
	repended, err = store.RependJobKillRowWithoutNotice(ctx, "owner-1", "call-named", named.ClaimID)
	require.NoError(t, err)
	require.False(t, repended, "a job_kill row that names its message must not be re-pended")
	row, err = store.Get(ctx, "owner-1", "call-named")
	require.NoError(t, err)
	require.Equal(t, "done", row.Delivery)
}
