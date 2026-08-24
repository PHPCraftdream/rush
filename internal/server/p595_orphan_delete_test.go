package server

// Task #595 follow-up: operator parity for orphaned assistant messages.
//
// After message.Service.Delete started refusing unfinished assistant messages
// (ErrMessageStillStreaming), handleRerunMessage added an orphan-rescue branch:
// when Delete fails with that error, if AgentCoordinator.IsSessionBusy returns
// false, ForceDelete the orphan. This file brings the same rescue logic to the
// operator-facing delete handlers (handleDeleteMessage and handleDeleteMessages).
//
// The rescue decision lives in the handler layer because message.Service cannot
// depend on agent.Coordinator (dependency direction: agent → message). IsSessionBusy
// is the same discriminator handleRerunMessage uses. Nil coordinator fails closed:
// "no coordinator to prove idle" must not weaken the streaming guard.
//
// Tests:
// A: busy session → delete of unfinished assistant message still refused
// B: idle session → orphaned unfinished assistant message IS deleted
// C: bulk delete_messages with one finished + one orphan → both deleted
// D: coordinator nil → delete of unfinished assistant message refused

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestHandleDeleteMessage_OrphanRescue_BusySession_RefusesDelete verifies
// that when the session is busy, an unfinished assistant message is still
// refused with EventError (the existing behavior). The rescue does not
// weaken the streaming guard — it only applies when the session is proven idle.
//
// This test pins the existing refusal behavior; no revert-check needed.
func TestHandleDeleteMessage_OrphanRescue_BusySession_RefusesDelete(t *testing.T) {
	a, sessionID := newMessageEditTestApp(t)
	ctx := context.Background()

	// Create an unfinished assistant message (orphan candidate).
	created, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "still streaming"}},
	})
	require.NoError(t, err)
	require.False(t, created.IsFinished(), "precondition: freshly created assistant message has no Finish part")

	// Mock coordinator as busy (session still active).
	a.AgentCoordinator = &mockAgentCoordinator{
		busy: true,
	}

	c := &Client{send: make(chan []byte, 4)}
	payload, err := json.Marshal(DeleteMessagePayload{MessageID: created.ID})
	require.NoError(t, err)

	handleDeleteMessage(ctx, a, c, WSMessage{ID: "req-1", Type: CmdDeleteMessage, Payload: payload})

	env := decodeReply(t, c)
	require.Equal(t, EventError, env.Type,
		"delete must be refused when session is busy, even though the message is unfinished")
	require.Contains(t, env.Error, "streaming")

	stored, getErr := a.Messages.Get(ctx, created.ID)
	require.NoError(t, getErr, "row must still exist: the handler must refuse the delete")
	require.Equal(t, "still streaming", stored.FullText())
}

// TestHandleDeleteMessage_OrphanRescue_IdleSession_DeletesOrphan verifies that
// when the session is idle, an unfinished assistant message (a true orphan from
// a crashed/killed turn) IS deleted successfully via the rescue branch.
//
// Revert-check: revert the orphan-rescue branch in deleteMessageRescuingOrphan
// (make it just return the Delete error instead of attempting rescue) and this
// test fails:
//   - Revert fragment in deleteMessageRescuingOrphan: after the `if !errors.Is(err, message.ErrMessageStillStreaming)`
//     block, add `return err` (skip the entire rescue branch).
//   - Expected failure: EventError response with "streaming" in the error,
//     and the row still present in the database.
//   - After restoring, the test passes: EventResponse ok, row gone.
//
// Task #622 note: this test now uses newAttachmentsTestApp (full app with
// a config store) rather than newMessageEditTestApp: the orphan rescue
// fails closed when external ownership cannot be verified (nil store = no
// data directory), so a minimal app would be refused by design.
func TestHandleDeleteMessage_OrphanRescue_IdleSession_DeletesOrphan(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	sess, err := a.Sessions.Create(t.Context(), "orphan-rescue-idle")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := context.Background()

	// Create an unfinished assistant message (orphan candidate).
	created, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "orphaned partial response"}},
	})
	require.NoError(t, err)
	require.False(t, created.IsFinished(), "precondition: freshly created assistant message has no Finish part")

	// Mock coordinator as idle (session not running any turn).
	a.AgentCoordinator = &mockAgentCoordinator{
		busy: false,
	}

	c := &Client{send: make(chan []byte, 4)}
	payload, err := json.Marshal(DeleteMessagePayload{MessageID: created.ID})
	require.NoError(t, err)

	handleDeleteMessage(ctx, a, c, WSMessage{ID: "req-1", Type: CmdDeleteMessage, Payload: payload})

	env := decodeReply(t, c)
	require.Equal(t, EventResponse, env.Type,
		"delete must succeed for an orphan in an idle session (rescue branch)")

	_, getErr := a.Messages.Get(ctx, created.ID)
	require.Error(t, getErr, "row must actually be gone after rescue")
}

// TestHandleDeleteMessages_OrphanRescue_IdleSession_DeletesBoth verifies that
// bulk delete_messages with one finished assistant message and one orphan
// (unfinished) in an idle session deletes BOTH successfully. The orphan is
// rescued, and the finished one is deleted normally. The handler replies ok
// with no partial-failure report.
//
// Revert-check: revert the orphan-rescue branch in deleteMessageRescuingOrphan
// (make it just return the Delete error) and this test fails:
//   - Revert fragment: same as TestHandleDeleteMessage_OrphanRescue_IdleSession_DeletesOrphan.
//   - Expected failure: EventError response with partial failure report
//     (the orphan is reported as failed), and the orphan row still exists.
//   - After restoring, the test passes: EventResponse ok, both rows gone.
func TestHandleDeleteMessages_OrphanRescue_IdleSession_DeletesBoth(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv
	// (task #622: full app required — see the single-delete test's note).
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	sess, err := a.Sessions.Create(t.Context(), "orphan-rescue-bulk")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := context.Background()

	// Create a finished assistant message.
	finished, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "finished message"}},
	})
	require.NoError(t, err)
	finished.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, finished))

	// Create an unfinished assistant message (orphan candidate).
	orphan, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "orphaned partial"}},
	})
	require.NoError(t, err)
	require.False(t, orphan.IsFinished(), "precondition: orphan has no Finish part")

	// Mock coordinator as idle.
	a.AgentCoordinator = &mockAgentCoordinator{
		busy: false,
	}

	c := &Client{send: make(chan []byte, 4)}
	payload, err := json.Marshal(DeleteMessagesPayload{MessageIDs: []string{finished.ID, orphan.ID}})
	require.NoError(t, err)

	handleDeleteMessages(ctx, a, c, WSMessage{ID: "req-1", Type: CmdDeleteMessages, Payload: payload})

	env := decodeReply(t, c)
	require.Equal(t, EventResponse, env.Type,
		"bulk delete must succeed: finished message deleted, orphan rescued")

	// Both rows must be gone.
	_, getErr := a.Messages.Get(ctx, finished.ID)
	require.Error(t, getErr, "finished message should have been deleted")

	_, getErr = a.Messages.Get(ctx, orphan.ID)
	require.Error(t, getErr, "orphan should have been force-deleted")
}

// TestHandleDeleteMessage_OrphanRescue_NilCoordinator_FailsClosed verifies that
// when AgentCoordinator is nil, the handler fails closed: an unfinished assistant
// message is refused with EventError. Nil coordinator means "no way to prove idle",
// so we must not weaken the streaming guard.
//
// This test pins the fail-closed behavior; no revert-check needed.
func TestHandleDeleteMessage_OrphanRescue_NilCoordinator_FailsClosed(t *testing.T) {
	a, sessionID := newMessageEditTestApp(t)
	ctx := context.Background()

	// Create an unfinished assistant message.
	created, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "unfinished"}},
	})
	require.NoError(t, err)
	require.False(t, created.IsFinished(), "precondition: freshly created assistant message has no Finish part")

	// Explicitly set coordinator to nil (default from newMessageEditTestApp).
	a.AgentCoordinator = nil

	c := &Client{send: make(chan []byte, 4)}
	payload, err := json.Marshal(DeleteMessagePayload{MessageID: created.ID})
	require.NoError(t, err)

	handleDeleteMessage(ctx, a, c, WSMessage{ID: "req-1", Type: CmdDeleteMessage, Payload: payload})

	env := decodeReply(t, c)
	require.Equal(t, EventError, env.Type,
		"delete must be refused when coordinator is nil (fail-closed)")
	require.Contains(t, env.Error, "streaming")

	stored, getErr := a.Messages.Get(ctx, created.ID)
	require.NoError(t, getErr, "row must still exist: the handler must refuse the delete")
	require.Equal(t, "unfinished", stored.FullText())
}
