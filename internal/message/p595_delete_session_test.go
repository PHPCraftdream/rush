package message

// Task #595: DeleteSessionMessages must bypass the DeleteMessageIfTerminal
// predicate. The per-row Delete refuses to delete an unfinished assistant
// message (role == 'assistant' AND finished_at IS NULL AND is_summary_message = 0)
// because such a row is still owned by an in-flight agent turn. However,
// DeleteSessionMessages is called by `crush sessions reset --force` AFTER
// the lock holder has been killed — that orphaned streaming row will never
// receive a terminal Finish, so the predicate would strand it forever.
//
// The fix is to use the unconditional DeleteSessionMessages query and publish
// DeletedEvents for all messages that existed at the start of the call.
//
// This file tests that DeleteSessionMessages successfully deletes a session
// containing a user message and an unfinished assistant message, and publishes
// DeletedEvents for both.

import (
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// TestDeleteSessionMessages_WithUnfinishedAssistantMessage proves that
// DeleteSessionMessages can wipe a session containing a user message and an
// unfinished assistant message (the exact shape that `crush sessions reset
// --force` encounters after SIGKILL'ing a live turn). It must:
//  1. Return nil (no error).
//  2. Leave zero messages in the session (verified via List/Count).
//  3. Publish DeletedEvents for both messages (verified by subscribing
//     before the call and draining the expected events).
//
// Revert-check: restore the old per-row Delete loop in DeleteSessionMessages
// and this test fails with ErrMessageStillStreaming and/or leftover rows.
func TestDeleteSessionMessages_WithUnfinishedAssistantMessage(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()
	sessionID := "test-session-delete-session-unfinished"

	// Subscribe BEFORE creating messages so we observe all events.
	sub := svc.Subscribe(ctx)

	// Create a user message (auto-gets a Finish part, so it's deletable by
	// the regular Delete path).
	userMsg, err := svc.Create(ctx, sessionID, CreateMessageParams{
		Role:  User,
		Parts: []ContentPart{TextContent{Text: "user input"}},
	})
	require.NoError(t, err)

	// Drain the user message's CreatedEvent.
	select {
	case <-sub:
	case <-time.After(drainTimeout):
		t.Fatal("did not receive CreatedEvent for user message")
	}

	// Create an assistant message with NO Finish part — this is the
	// "still streaming" shape that the regular Delete would refuse.
	assistantMsg := mustCreateAssistant(t, svc, sessionID, "assistant response, not finished")

	// Drain the assistant message's CreatedEvent.
	select {
	case <-sub:
	case <-time.After(drainTimeout):
		t.Fatal("did not receive CreatedEvent for assistant message")
	}

	// Verify the session has 2 messages and the assistant is unfinished.
	messages, err := svc.List(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, messages, 2, "session must start with 2 messages")
	require.False(t, assistantMsg.IsFinished(), "precondition: assistant message is not finished")

	// Call DeleteSessionMessages — this must succeed despite the unfinished
	// assistant message.
	err = svc.DeleteSessionMessages(ctx, sessionID)
	require.NoError(t, err, "DeleteSessionMessages must succeed even with an unfinished assistant message")

	// Verify both messages are gone.
	messagesAfter, err := svc.List(ctx, sessionID)
	require.NoError(t, err)
	require.Empty(t, messagesAfter, "all messages must be deleted")

	count, err := svc.Count(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, int64(0), count, "message count must be 0")

	// Verify DeletedEvents were published for both messages.
	// We expect 2 DeletedEvents (one for each message).
	deletedCount := 0
	deletedUser := false
	deletedAssistant := false
	for deletedCount < 2 {
		select {
		case ev := <-sub:
			if ev.Type == pubsub.DeletedEvent {
				deletedCount++
				if ev.Payload.ID == userMsg.ID {
					deletedUser = true
				}
				if ev.Payload.ID == assistantMsg.ID {
					deletedAssistant = true
				}
			}
		case <-time.After(drainTimeout):
			t.Fatalf("expected 2 DeletedEvents, got %d", deletedCount)
		}
	}
	require.True(t, deletedUser, "DeletedEvent must be published for user message")
	require.True(t, deletedAssistant, "DeletedEvent must be published for assistant message")
}

// TestDeleteSessionMessages_EmptySession is the control: deleting an empty
// session (no messages) must be a no-op that returns nil, not an error.
func TestDeleteSessionMessages_EmptySession(t *testing.T) {
	_, q := newTestMessageDB(t)
	svc := NewService(q).(*service)
	ctx := t.Context()
	sessionID := "test-session-delete-session-empty"

	// Verify the session is empty.
	messages, err := svc.List(ctx, sessionID)
	require.NoError(t, err)
	require.Empty(t, messages, "session must start empty")

	// DeleteSessionMessages on an empty session must succeed.
	err = svc.DeleteSessionMessages(ctx, sessionID)
	require.NoError(t, err, "deleting an empty session must be a no-op that returns nil")

	// Session must still be empty.
	messagesAfter, err := svc.List(ctx, sessionID)
	require.NoError(t, err)
	require.Empty(t, messagesAfter, "session must remain empty")
}

// drainTimeout is the deadline for waiting for events in the tests above.
// 500ms is generous: pubsub.PublishMustDeliver blocks per-subscriber with a
// bounded timeout (currently 100ms per subscriber), so this test will fail
// quickly if the publish is broken.
const drainTimeout = 500 * time.Millisecond
