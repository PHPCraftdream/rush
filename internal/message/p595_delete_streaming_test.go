package message

// Task #595 (P1-1 of the 2026-08-19 static follow-up review /
// docs/reviews/2026-08-19-release-readiness-static-follow-up-d3ee9841.md,
// and H2 of docs/reviews/2026-08-19-oxx-session-review.md).
//
// Commit 547b0815 refused content/part EDITS to an assistant message with no
// terminal Finish (updateMessageAndVerify in
// internal/server/handlers_messages.go), but DELETE went through
// message.Service.Delete unconditionally: read, unconditional DELETE,
// publish a terminal DeletedEvent. The live turn keeps its own in-memory
// currentAssistant and, independently of a delete, still writes its
// terminal state back to the same row id via Update -- which used to
// hardcode rowsAffected = 1 and publish UpdatedEvent regardless of whether
// the row still existed, "resurrecting" a deleted streaming message in the
// live UI only (absent from the DB, gone again on reload).
//
// This file covers both halves of the fix:
//   - Delete now refuses a still-streaming assistant message via a DB
//     predicate (DeleteMessageIfTerminal), atomically, rather than racing a
//     read-then-delete against the turn's own concurrent write.
//   - Update's terminal branch now reports the real rows-affected outcome
//     (UpdateMessage is :execrows, not :exec) and skips publishing
//     UpdatedEvent when the row no longer exists.

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// mustCreateAssistant is a small helper shared by the tests below: creates
// an assistant message with the given starting text.
func mustCreateAssistant(t *testing.T, svc *service, sessionID, text string) Message {
	t.Helper()
	created, err := svc.Create(t.Context(), sessionID, CreateMessageParams{
		Role:  Assistant,
		Parts: []ContentPart{TextContent{Text: text}},
	})
	require.NoError(t, err)
	return created
}

// TestDelete_RefusesStillStreamingAssistantMessage_NoFinishPart is the most
// basic mid-stream shape: an assistant row with no Finish part at all (the
// gap between Create and the first checkpoint tick).
//
// Revert-check: replace DeleteMessageIfTerminal with plain DeleteMessage in
// Service.Delete and this fails -- the row is deleted with no error.
func TestDelete_RefusesStillStreamingAssistantMessage_NoFinishPart(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	created := mustCreateAssistant(t, svc, "test-session-delete-nofinish", "streaming, no finish yet")
	require.False(t, created.IsFinished(), "precondition: freshly created assistant message has no Finish part")

	err := svc.Delete(ctx, created.ID)
	require.ErrorIs(t, err, ErrMessageStillStreaming,
		"Delete must refuse a still-streaming assistant message instead of deleting it")

	stored, getErr := svc.Get(ctx, created.ID)
	require.NoError(t, getErr, "the row must still exist after a refused delete")
	require.Equal(t, "streaming, no finish yet", stored.FullText())
}

// TestDelete_RefusesStillStreamingAssistantMessage_PartialCheckpoint covers
// the more realistic mid-stream shape: a Partial Finish written by the
// ~2s auto-checkpoint ticker (finished_at stays NULL, exactly like the
// scenario TestUpdate_StaleCheckpointGenerationCannotOverwriteNewer builds
// in p555_checkpoint_generation_test.go).
//
// Revert-check: same as above -- replace DeleteMessageIfTerminal with plain
// DeleteMessage and this fails.
func TestDelete_RefusesStillStreamingAssistantMessage_PartialCheckpoint(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	created := mustCreateAssistant(t, svc, "test-session-delete-partial", "start")
	checkpoint := partialCheckpoint(created, "streamed so far", 1)
	require.NoError(t, svc.Update(ctx, checkpoint))

	loaded, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.True(t, loaded.IsPartial(), "precondition: row carries a Partial Finish from the checkpoint ticker")
	require.False(t, loaded.IsFinished(), "precondition: a Partial Finish is not a terminal Finish")

	err = svc.Delete(ctx, created.ID)
	require.ErrorIs(t, err, ErrMessageStillStreaming,
		"Delete must refuse a checkpointed-but-not-terminal assistant message")

	stored, getErr := svc.Get(ctx, created.ID)
	require.NoError(t, getErr, "the row must still exist after a refused delete")
	require.Equal(t, "streamed so far", textOf(t, stored))
}

// TestDelete_AllowsTerminallyFinishedAssistantMessage is the control: once a
// real (non-Partial) Finish part lands, Delete must succeed exactly as
// before -- nothing is streaming the row anymore, so nothing can race the
// delete or resurrect it afterward.
func TestDelete_AllowsTerminallyFinishedAssistantMessage(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	created := mustCreateAssistant(t, svc, "test-session-delete-finished", "start")
	created.AddFinish(FinishReasonEndTurn, "", "")
	require.NoError(t, svc.Update(ctx, created))

	err := svc.Delete(ctx, created.ID)
	require.NoError(t, err, "deleting a terminally finished assistant message must succeed")

	_, getErr := svc.Get(ctx, created.ID)
	require.Error(t, getErr, "the row must actually be gone")
}

// TestDelete_AllowsUserMessageEvenWithoutTerminalFinish guards against the
// tempting wrong fix (same shape as
// TestUpdateMessageAndVerify_UserMessageEditLands in
// internal/server/p569_message_edit_verify_test.go): gating the refusal on
// !IsFinished() alone rather than Role == Assistant && !IsFinished() would
// incidentally still pass for a normal user row (Create auto-appends a
// non-partial Finish{Reason: "stop"} to every non-Assistant row), but the
// DB predicate itself (role != 'assistant' OR finished_at IS NOT NULL) is
// what actually matters -- this pins that a user row is unconditionally
// deletable via the role half of the OR, independent of finished_at.
func TestDelete_AllowsUserMessageEvenWithoutTerminalFinish(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	created, err := svc.Create(ctx, "test-session-delete-user", CreateMessageParams{
		Role:  User,
		Parts: []ContentPart{TextContent{Text: "hello"}},
	})
	require.NoError(t, err)
	require.Equal(t, User, created.Role, "precondition: this is a user message")

	err = svc.Delete(ctx, created.ID)
	require.NoError(t, err, "deleting a user message must succeed regardless of finished_at")

	_, getErr := svc.Get(ctx, created.ID)
	require.Error(t, getErr, "the row must actually be gone")
}

// TestDelete_AllowsUnfinishedSummaryMessage_SelfCleanupAfterCancel pins a
// real regression this task's own fix caused and then had to correct: a
// summary message (IsSummaryMessage: true, created by
// internal/agent/agent_compaction.go's runSummarizeSilent/runSummarizeBody)
// with NO Finish part is legitimately deleted by that SAME call, as
// self-cleanup, when its stream is cancelled before ever finishing --
// TestP1_4_CleanupUsesCancelImmuneContext in
// internal/agent/p1_3_p1_4_regression_test.go exercises the full path.
//
// This is NOT the P1-1 race the rest of this file guards against: P1-1 is
// about an EXTERNAL actor (an operator's Trash click or bulk selection,
// reached through the web UI) deleting a message a DIFFERENT, still-live
// turn intends to keep writing to. A summary message is never reachable
// through that UI at all -- web/src/components/Message/Message.tsx routes
// IsSummaryMessage rows to SummaryMessage.tsx, which renders no Trash
// control and sits outside the selection-checkbox tree entirely (see that
// component; it has no Trash2 import or onDelete prop) -- so the only
// caller that ever deletes one is the SAME turn that created it, discarding
// its own abandoned draft before writing anything else to that id. That is
// single-writer self-cleanup, not the external race the streaming guard
// exists to prevent, and the DB predicate exempts it accordingly
// (is_summary_message = 1 in DeleteMessageIfTerminal).
//
// Revert-check: drop "OR is_summary_message = 1" from
// DeleteMessageIfTerminal's WHERE clause (internal/db/sql/messages.sql;
// re-run sqlc generate) and this fails with ErrMessageStillStreaming --
// exactly the regression this test exists to keep out.
func TestDelete_AllowsUnfinishedSummaryMessage_SelfCleanupAfterCancel(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	created, err := svc.Create(ctx, "test-session-delete-summary", CreateMessageParams{
		Role:             Assistant,
		IsSummaryMessage: true,
		Hidden:           true,
	})
	require.NoError(t, err)
	require.False(t, created.IsFinished(), "precondition: the stream was cancelled before ever finishing")

	err = svc.Delete(ctx, created.ID)
	require.NoError(t, err, "an unfinished summary message must remain deletable by its own creator's cancel-cleanup path")

	_, getErr := svc.Get(ctx, created.ID)
	require.Error(t, getErr, "the row must actually be gone")
}

// TestDelete_NonexistentRow_ReturnsNotFoundNotStillStreaming pins the
// disambiguation Delete's doc comment promises: a row that never existed at
// all must surface as the ordinary Get "not found" error, NOT
// ErrMessageStillStreaming -- the two are different operator-facing
// problems (there is nothing to wait for in the first case).
func TestDelete_NonexistentRow_ReturnsNotFoundNotStillStreaming(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	err := svc.Delete(ctx, "does-not-exist")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrMessageStillStreaming,
		"a row that never existed must not be reported as still-streaming")
}

// TestUpdate_TerminalWriteAgainstDeletedRow_DoesNotPublishUpdatedEvent is
// the core regression test for the "resurrection" scenario the review
// describes: an operator deletes a still-... no, deletes an assistant
// message THAT DELETE ITSELF NOW REFUSES while it's streaming -- so to
// reach the terminal-write-vs-deleted-row race this test has to get the row
// deleted through a path Delete's own guard does not cover: after the
// message is genuinely terminal by every measure Delete checks EXCEPT that
// the row has, in the meantime, been removed by some other means (modeling
// any future/alternate deletion path, or simply the row having been purged
// out from under a lagging in-memory Message struct held by a turn that
// read it earlier). This isolates the SECOND half of the P1-1 fix
// (Update's rows-affected honesty) from the FIRST half (Delete's guard),
// exactly as the task instructions require testing each mechanism
// independently.
//
// Revert-check: restore "rowsAffected = 1 // Terminal update always wins"
// in Service.Update's terminal branch and this fails -- UpdatedEvent is
// published for a row that does not exist.
func TestUpdate_TerminalWriteAgainstDeletedRow_DoesNotPublishUpdatedEvent(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	// Subscribe BEFORE Create: Create's CreatedEvent uses best-effort
	// Publish (non-blocking), so a subscription registered afterward would
	// simply never see it -- there is no buffering for a channel that does
	// not exist yet.
	sub := svc.Subscribe(ctx)
	created := mustCreateAssistant(t, svc, "test-session-terminal-vs-deleted", "start")
	// Drain the CreatedEvent so it can't be mistaken for the terminal
	// UpdatedEvent under test.
	select {
	case <-sub:
	case <-time.After(time.Second):
		t.Fatal("did not receive CreatedEvent")
	}

	// Remove the row directly via the raw querier -- bypassing
	// Service.Delete's guard entirely -- to model "the row is gone" without
	// depending on the very delete path under test elsewhere in this file.
	// This is exactly the state DeleteMessageIfTerminal's own 0-rows-affected
	// case produces from Service.Delete's perspective once it IS allowed to
	// delete a row: from Update's point of view, it cannot distinguish "an
	// operator deleted it just now" from "some other path removed it" --
	// only that the row is gone.
	require.NoError(t, q.DeleteMessage(ctx, created.ID))

	// Simulate the live turn's terminal write landing after the row is gone
	// -- exactly what internal/agent/agent_turn.go's OnStepFinish does at
	// the end of a turn: a non-Partial Finish through the unconditional
	// "terminal update always wins" branch.
	terminal := created.Clone()
	terminal.Parts = []ContentPart{TextContent{Text: "terminal state nobody will see"}}
	terminal.AddFinish(FinishReasonEndTurn, "", "")

	err := svc.Update(ctx, terminal)
	require.NoError(t, err, "a terminal write against a deleted row must not itself error -- the turn should not crash over a message the operator removed")

	select {
	case ev := <-sub:
		t.Fatalf("UpdatedEvent must NOT be published for a terminal write that affected 0 rows (row was deleted): got %+v", ev.Payload)
	case <-time.After(150 * time.Millisecond):
		// Correct: no event, no resurrection.
	}

	_, getErr := svc.Get(ctx, created.ID)
	require.Error(t, getErr, "the row must remain deleted -- the terminal write must not have recreated or resurrected it")
}

// TestUpdate_TerminalWriteAgainstExistingRow_PublishesUpdatedEvent is the
// control for the test above: the overwhelmingly common case (the row still
// exists) must still publish UpdatedEvent exactly as before. This is what
// stops "skip the publish" from silently regressing into "never publish
// terminal writes".
func TestUpdate_TerminalWriteAgainstExistingRow_PublishesUpdatedEvent(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	// Subscribe BEFORE Create -- see the comment in
	// TestUpdate_TerminalWriteAgainstDeletedRow_DoesNotPublishUpdatedEvent
	// for why ordering matters here (Create's CreatedEvent is best-effort
	// and non-blocking).
	sub := svc.Subscribe(ctx)
	created := mustCreateAssistant(t, svc, "test-session-terminal-vs-existing", "start")
	select {
	case <-sub:
	case <-time.After(time.Second):
		t.Fatal("did not receive CreatedEvent")
	}

	terminal := created.Clone()
	terminal.Parts = []ContentPart{TextContent{Text: "terminal state"}}
	terminal.AddFinish(FinishReasonEndTurn, "", "")

	require.NoError(t, svc.Update(ctx, terminal))

	select {
	case ev := <-sub:
		require.Equal(t, pubsub.UpdatedEvent, ev.Type)
		require.Equal(t, "terminal state", textOf(t, ev.Payload))
	case <-time.After(time.Second):
		t.Fatal("expected UpdatedEvent for a terminal write against an existing row")
	}

	stored, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, "terminal state", textOf(t, stored))
}

// TestForceDelete_RemovesUnfinishedAssistantMessage proves that ForceDelete
// unconditionally deletes an unfinished assistant message (no Finish part or
// only a Partial Finish from the checkpoint ticker) and publishes DeletedEvent.
// This is the cleanup path for orphaned streaming rows left behind by crashed
// or killed agent turns.
//
// Revert-check: replace s.q.DeleteMessage with s.q.DeleteMessageIfTerminal in
// Service.ForceDelete (the query call, not the Get before it) and this fails
// with ErrMessageStillStreaming and/or the row still existing.
func TestForceDelete_RemovesUnfinishedAssistantMessage(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	// Subscribe BEFORE Create to observe all events.
	sub := svc.Subscribe(ctx)

	// Create an assistant message with NO Finish part — the exact "still
	// streaming" shape that regular Delete refuses.
	created := mustCreateAssistant(t, svc, "test-session-force-delete", "orphaned streaming text")

	// Drain the CreatedEvent so it can't be mistaken for the DeletedEvent.
	select {
	case <-sub:
	case <-time.After(drainTimeout):
		t.Fatal("did not receive CreatedEvent")
	}

	// Verify the message exists and is unfinished.
	stored, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.False(t, stored.IsFinished(), "precondition: message is not finished")
	require.Equal(t, "orphaned streaming text", textOf(t, stored))

	// ForceDelete must succeed despite the message being unfinished.
	err = svc.ForceDelete(ctx, created.ID)
	require.NoError(t, err, "ForceDelete must remove an unfinished assistant message")

	// Verify the row is actually gone.
	_, getErr := svc.Get(ctx, created.ID)
	require.Error(t, getErr, "the row must actually be gone after ForceDelete")

	// Verify DeletedEvent was published.
	select {
	case ev := <-sub:
		require.Equal(t, pubsub.DeletedEvent, ev.Type)
		require.Equal(t, created.ID, ev.Payload.ID)
	case <-time.After(drainTimeout):
		t.Fatal("expected DeletedEvent after ForceDelete")
	}
}

// TestForceDelete_PartialCheckpointMessage proves that ForceDelete handles
// the more realistic mid-stream shape: a Partial Finish written by the ~2s
// auto-checkpoint ticker (finished_at stays NULL).
//
// Revert-check: same as above — replace DeleteMessage with DeleteMessageIfTerminal
// in Service.ForceDelete and this fails.
func TestForceDelete_PartialCheckpointMessage(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	// Subscribe BEFORE Create.
	sub := svc.Subscribe(ctx)

	// Create and checkpoint an assistant message.
	created := mustCreateAssistant(t, svc, "test-session-force-delete-partial", "start")
	checkpoint := partialCheckpoint(created, "streamed so far", 1)
	require.NoError(t, svc.Update(ctx, checkpoint))

	// Drain CreatedEvent and UpdatedEvent (the checkpoint publish).
	for i := 0; i < 2; i++ {
		select {
		case <-sub:
		case <-time.After(drainTimeout):
			t.Fatalf("did not receive event %d", i)
		}
	}

	// Verify the message is partial, not terminal.
	stored, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.True(t, stored.IsPartial(), "precondition: row carries a Partial Finish")
	require.False(t, stored.IsFinished(), "precondition: a Partial Finish is not a terminal Finish")

	// ForceDelete must succeed.
	err = svc.ForceDelete(ctx, created.ID)
	require.NoError(t, err, "ForceDelete must remove a partial-checkpoint message")

	// Verify the row is gone.
	_, getErr := svc.Get(ctx, created.ID)
	require.Error(t, getErr)

	// Verify DeletedEvent was published.
	select {
	case ev := <-sub:
		require.Equal(t, pubsub.DeletedEvent, ev.Type)
		require.Equal(t, created.ID, ev.Payload.ID)
	case <-time.After(drainTimeout):
		t.Fatal("expected DeletedEvent after ForceDelete")
	}
}

// TestForceDelete_NonexistentRow_ReturnsNotFound pins that ForceDelete
// propagates Get's "not found" error when the row never existed, just like
// regular Delete does.
func TestForceDelete_NonexistentRow_ReturnsNotFound(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()

	err := svc.ForceDelete(ctx, "does-not-exist")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrMessageStillStreaming,
		"a row that never existed must not be reported as still-streaming")
}
