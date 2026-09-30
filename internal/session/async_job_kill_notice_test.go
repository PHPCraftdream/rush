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
	"context"
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
// production code's fix by removing the SetAsyncJobNoticeMessageIDForClaimIfDone
// call from AnnounceJobKillResult -- this test FAILED (row.NoticeMessageID.Valid
// was false, RerunTruncate found nothing). Restored; re-ran, passed.
func TestAnnounceJobKillResult_NamesTheRowAndSurvivesRerunRepend(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	messages := message.NewService(db.New(store.sqlDB))

	claimed, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
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
	killMsg, err := store.AnnounceJobKillResult(ctx, messages, "owner-1", claimed.Row.ClaimID, message.CreateMessageParams{
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
// the "0 rows affected is not an error" half of the defensive guard. Production
// passes a claim only after its own transition won, so the row here (finished
// on its own, delivery still 'pending') is a state the caller never reaches;
// it is forced to prove the guard: the tool-result message for job_kill's OWN
// call is still persisted -- every tool_use needs a tool_result -- and
// notice_message_id is left alone (SetAsyncJobNoticeMessageIDForClaimIfDone's
// guard), never pointing at a message that is not this row's own pulled notice.
func TestAnnounceJobKillResult_LostRaceStillPersistsMessageWithoutFusing(t *testing.T) {
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	messages := message.NewService(db.New(store.sqlDB))

	claimed, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call-1", Kind: JobKindCommand, Input: "x", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call-1"))

	// A DIFFERENT cause (natural finish) won first: delivery stays 'pending'.
	_, err = store.Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "call-1", State: "completed",
		ResultSummary: "finished on its own", Wake: true,
	})
	require.NoError(t, err)

	msg, err := store.AnnounceJobKillResult(ctx, messages, "owner-1", claimed.Row.ClaimID, message.CreateMessageParams{
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
	repended, err := store.RependJobKillRowWithoutNotice(ctx, "owner-1", "some-other-claim")
	require.NoError(t, err)
	require.False(t, repended, "another claim's id must not re-pend this row")

	repended, err = store.RependJobKillRowWithoutNotice(ctx, "owner-1", lost.ClaimID)
	require.NoError(t, err)
	require.True(t, repended)
	row, err := store.Get(ctx, "owner-1", "call-lost")
	require.NoError(t, err)
	require.Equal(t, "pending", row.Delivery)
	require.EqualValues(t, 0, row.Reacted)
	require.EqualValues(t, 0, row.Wake)

	repended, err = store.RependJobKillRowWithoutNotice(ctx, "owner-1", lost.ClaimID)
	require.NoError(t, err)
	require.False(t, repended, "the repair is idempotent: a pending row is not touched again")

	pulled, err := store.PullJobNotices(ctx, messages, "owner-1", buildTestJobNoticeParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1)
	require.Contains(t, pulled[0].Message.FullText(), "output of call-lost")
	require.False(t, pulled[0].Wake, "the repaired job_kill notice never wakes")

	// A row that already names its result message is left alone.
	named := kill("call-named")
	_, err = store.AnnounceJobKillResult(ctx, messages, "owner-1", named.ClaimID, message.CreateMessageParams{
		Role: message.Tool, Parts: []message.ContentPart{message.TextContent{Text: "stopped"}},
	})
	require.NoError(t, err)
	repended, err = store.RependJobKillRowWithoutNotice(ctx, "owner-1", named.ClaimID)
	require.NoError(t, err)
	require.False(t, repended, "a job_kill row that names its message must not be re-pended")
	row, err = store.Get(ctx, "owner-1", "call-named")
	require.NoError(t, err)
	require.Equal(t, "done", row.Delivery)
}

// killReusedKeyScenario is R3A-2's exact interleaving: job_kill's transition
// has committed done/reacted=1 on J1 (tool_call_id "call_0"), and BEFORE its
// result message is written the model's next response claims "call_0" again
// (per-response numbering): J1 is archived to "call_0#reused#<uuid>" and J2
// owns "call_0". The fused write / re-pend must still find J1.
type killReusedKeyScenario struct {
	store    *AsyncJobStore
	messages message.Service
	killed   db.AsyncJob // J1 as job_kill's transition left it
	archived string      // J1's tool_call_id after the archive
	fresh    db.AsyncJob // J2
}

func newKillReusedKeyScenario(t *testing.T) *killReusedKeyScenario {
	t.Helper()
	store, q, ctx := newTestStore(t)
	require.NoError(t, seedSession(ctx, q, "owner-1"))
	s := &killReusedKeyScenario{store: store, messages: message.NewService(db.New(store.sqlDB))}

	first, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call_0", Kind: JobKindCommand, Input: "first", ToolName: "bash"})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, "owner-1", "call_0"))
	res, err := store.Transition(ctx, TransitionParams{
		Owner: "owner-1", ToolCallID: "call_0", ClaimID: first.Row.ClaimID, State: "cancelled", NoticeKind: "job_kill",
		ResultSummary: "partial output of the first job", Delivery: "done", Reacted: true,
	})
	require.NoError(t, err)
	require.Equal(t, TransitionWon, res.Outcome)
	s.killed = res.Row

	second, err := store.Claim(ctx, ClaimParams{Owner: "owner-1", ToolCallID: "call_0", Kind: JobKindCommand, Input: "second", ToolName: "bash"})
	require.NoError(t, err)
	require.False(t, second.Existing)
	require.NotEqual(t, first.Row.ClaimID, second.Row.ClaimID)
	s.fresh = second.Row

	rows, err := store.q.ListAsyncJobsForOwner(ctx, "owner-1")
	require.NoError(t, err)
	for _, r := range rows {
		if r.ClaimID == first.Row.ClaimID {
			s.archived = r.ToolCallID
		}
	}
	require.Contains(t, s.archived, archivedToolCallIDMarker, "the new claim must have archived the killed row")
	return s
}

func (s *killReusedKeyScenario) row(t *testing.T, toolCallID string) db.AsyncJob {
	t.Helper()
	row, err := s.store.Get(context.Background(), "owner-1", toolCallID)
	require.NoError(t, err)
	return row
}

func killResultParams() message.CreateMessageParams {
	return message.CreateMessageParams{
		Role: message.Tool, Parts: []message.ContentPart{message.TextContent{Text: "Async job call_0 (bash) was stopped (job_kill)."}},
	}
}

// TestAnnounceJobKillResult_ReusedToolCallID_NamesTheKilledRowNotTheNewOne: the
// fused write is keyed by the killed claim, so J1 -- now under its archived
// key -- names job_kill's result message and the new J2 is untouched. Then
// dead-host recovery must not re-pend J1 (the model already has its output in
// the job_kill result: re-pending would deliver it twice) and a Rerun past the
// job_kill message must be able to re-pend it.
//
// Revert-check: key SetAsyncJobNoticeMessageIDForClaimIfDone by tool_call_id
// (owner + the reused id, the pre-fix query) -> J1 keeps a NULL
// notice_message_id (the write matches J2, which is 'running', 0 rows), the
// first assertion fails; recovery then re-pends J1 (second assertion).
func TestAnnounceJobKillResult_ReusedToolCallID_NamesTheKilledRowNotTheNewOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newKillReusedKeyScenario(t)

	msg, err := s.store.AnnounceJobKillResult(ctx, s.messages, "owner-1", s.killed.ClaimID, killResultParams())
	require.NoError(t, err)

	j1 := s.row(t, s.archived)
	require.True(t, j1.NoticeMessageID.Valid, "DUR-11: the killed row must name the message that carries its result")
	require.Equal(t, msg.ID, j1.NoticeMessageID.String)
	require.Equal(t, "done", j1.Delivery)
	j2 := s.row(t, "call_0")
	require.False(t, j2.NoticeMessageID.Valid, "the new claim under the reused id must not be named")
	require.Equal(t, s.fresh.ClaimID, j2.ClaimID)
	require.Equal(t, "running", j2.State)

	// The host dies. J1 already names its message: recovery must leave it done.
	q := db.New(s.store.sqlDB)
	fabricateDeadHost(t, ctx, q, s.store.dataDir, "dead-host-r3a2")
	_, err = s.store.sqlDB.ExecContext(ctx, `UPDATE async_jobs SET host_id = 'dead-host-r3a2' WHERE owner_session_id = 'owner-1'`)
	require.NoError(t, err)
	require.NoError(t, s.store.MarkAnnounced(ctx, "owner-1", "call_0"))
	out, err := s.store.SweepDeadHosts(ctx, nil)
	require.NoError(t, err)
	require.Zero(t, out["dead-host-r3a2"].Repended, "a job_kill row that names its message is not re-pended by recovery")
	require.Equal(t, "done", s.row(t, s.archived).Delivery)

	pulled, err := s.store.PullJobNotices(ctx, s.messages, "owner-1", buildTestJobNoticeParams)
	require.NoError(t, err)
	require.Len(t, pulled, 1, "only the interrupted J2 is delivered; J1's output is not delivered a second time")
	require.NotContains(t, pulled[0].Message.FullText(), "partial output of the first job")

	// Rerun deletes job_kill's result message: J1 (named) is re-pended, J2 not.
	target, err := s.messages.Create(ctx, "owner-1", message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)
	_, err = TruncateForRerun(ctx, s.store.sqlDB, s.messages, RerunTruncateParams{
		Owner: "owner-1", TargetID: target.ID, TailIDs: []string{msg.ID},
	})
	require.NoError(t, err)
	require.Equal(t, "pending", s.row(t, s.archived).Delivery, "Rerun must be able to re-pend the archived job_kill row")
}

// TestRependJobKillRowWithoutNotice_ReusedToolCallID_RependsTheKilledRow: when
// the fused write does not happen, the live re-pend finds J1 by its claim
// although the tool_call_id it was killed under now belongs to J2; J2 is
// never touched.
//
// Revert-check: add `AND tool_call_id = @tool_call_id` (the pre-fix key, the
// reused id) to RependJobKillRowWithoutNotice -> matches nothing, J1 stays
// done with no message and require.True fails.
func TestRependJobKillRowWithoutNotice_ReusedToolCallID_RependsTheKilledRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newKillReusedKeyScenario(t)

	repended, err := s.store.RependJobKillRowWithoutNotice(ctx, "owner-1", s.killed.ClaimID)
	require.NoError(t, err)
	require.True(t, repended)
	j1 := s.row(t, s.archived)
	require.Equal(t, "pending", j1.Delivery)
	require.EqualValues(t, 0, j1.Reacted)
	require.EqualValues(t, 0, j1.Wake)
	j2 := s.row(t, "call_0")
	require.Equal(t, "running", j2.State)
	require.Equal(t, s.fresh.ClaimID, j2.ClaimID)

	repended, err = s.store.RependJobKillRowWithoutNotice(ctx, "owner-1", "")
	require.NoError(t, err)
	require.False(t, repended, "an empty claim id must match nothing (legacy rows carry claim_id '')")
}
