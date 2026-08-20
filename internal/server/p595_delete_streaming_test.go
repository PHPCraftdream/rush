package server

// Task #595 (P1-1 of the 2026-08-19 static follow-up review /
// docs/reviews/2026-08-19-release-readiness-static-follow-up-d3ee9841.md,
// and H2 of docs/reviews/2026-08-19-oxx-session-review.md).
//
// Commit 547b0815 refused content/part EDITS to an assistant message with no
// terminal Finish via updateMessageAndVerify, but the delete handlers next
// to it had no equivalent check: single delete called a.Messages.Delete
// directly, and bulk delete did the same per ID while swallowing individual
// errors into a slog.Warn and always replying {"status":"ok"}. This file
// covers the WS-handler-level half of the fix -- message.Service.Delete's
// own guard is covered independently in
// internal/message/p595_delete_streaming_test.go.
import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// decodeReply reads exactly one WSMessage off c.send and unmarshals it.
func decodeReply(t *testing.T, c *Client) WSMessage {
	t.Helper()
	select {
	case raw := <-c.send:
		var env WSMessage
		require.NoError(t, json.Unmarshal(raw, &env))
		return env
	default:
		t.Fatal("expected a reply on the client's send channel, got none")
		return WSMessage{}
	}
}

// TestHandleDeleteMessage_RefusesStillStreamingAssistantMessage is the
// server-side counterpart to the review's specific example: "a stale client
// sends a delete for a message that is still partial and must be refused."
//
// Revert-check: replace message.Service.Delete's DeleteMessageIfTerminal
// call with plain DeleteMessage (see
// internal/message/p595_delete_streaming_test.go's revert-check A for the
// verbatim failure this produces at the Service layer) and this test fails
// here too, because handleDeleteMessage has no guard of its own -- it
// relies entirely on a.Messages.Delete to refuse.
func TestHandleDeleteMessage_RefusesStillStreamingAssistantMessage(t *testing.T) {
	a, sessionID := newMessageEditTestApp(t)
	ctx := context.Background()

	created, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "still streaming"}},
	})
	require.NoError(t, err)
	require.False(t, created.IsFinished(), "precondition: freshly created assistant message has no Finish part")

	c := &Client{send: make(chan []byte, 4)}
	payload, err := json.Marshal(DeleteMessagePayload{MessageID: created.ID})
	require.NoError(t, err)

	handleDeleteMessage(ctx, a, c, WSMessage{ID: "req-1", Type: CmdDeleteMessage, Payload: payload})

	env := decodeReply(t, c)
	require.Equal(t, EventError, env.Type,
		"client was told the delete succeeded (or got no reply) for a message that is still streaming")
	require.Contains(t, env.Error, "streaming")

	stored, getErr := a.Messages.Get(ctx, created.ID)
	require.NoError(t, getErr, "row must still exist: the handler must refuse the delete, not delete-then-detect")
	require.Equal(t, "still streaming", stored.FullText())
}

// TestHandleDeleteMessage_AllowsTerminallyFinishedAssistantMessage is the
// control: once a real (non-Partial) Finish part lands, the single-delete
// handler must succeed exactly as before.
func TestHandleDeleteMessage_AllowsTerminallyFinishedAssistantMessage(t *testing.T) {
	a, sessionID := newMessageEditTestApp(t)
	ctx := context.Background()

	created, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "done"}},
	})
	require.NoError(t, err)
	created.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, created))

	c := &Client{send: make(chan []byte, 4)}
	payload, err := json.Marshal(DeleteMessagePayload{MessageID: created.ID})
	require.NoError(t, err)

	handleDeleteMessage(ctx, a, c, WSMessage{ID: "req-1", Type: CmdDeleteMessage, Payload: payload})

	env := decodeReply(t, c)
	require.Equal(t, EventResponse, env.Type, "deleting a terminally finished assistant message must succeed")

	_, getErr := a.Messages.Get(ctx, created.ID)
	require.Error(t, getErr, "row must actually be gone")
}

// TestHandleDeleteMessages_PartialFailureDoesNotReportOk covers the review's
// second specific example: "a bulk delete with one failing ID must not
// report success." Two messages are requested: one terminally finished
// (deletable) and one still streaming (refused by the Service guard). The
// handler must not reply {"status":"ok"} -- some of the requested work did
// not happen.
//
// Revert-check: restore handleDeleteMessages' old unconditional
// c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
// (deleting the failed-tracking branch entirely) and this test fails,
// because the reply becomes EventResponse regardless of the per-ID error.
func TestHandleDeleteMessages_PartialFailureDoesNotReportOk(t *testing.T) {
	a, sessionID := newMessageEditTestApp(t)
	ctx := context.Background()

	finished, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "finished one"}},
	})
	require.NoError(t, err)
	finished.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, finished))

	streaming, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "streaming one"}},
	})
	require.NoError(t, err)
	require.False(t, streaming.IsFinished(), "precondition: this one has no Finish part yet")

	c := &Client{send: make(chan []byte, 4)}
	payload, err := json.Marshal(DeleteMessagesPayload{MessageIDs: []string{finished.ID, streaming.ID}})
	require.NoError(t, err)

	handleDeleteMessages(ctx, a, c, WSMessage{ID: "req-1", Type: CmdDeleteMessages, Payload: payload})

	env := decodeReply(t, c)
	require.Equal(t, EventError, env.Type,
		"bulk delete must not report {status: ok} when one of the requested IDs failed to delete")
	require.NotEmpty(t, env.Error, "the failure must be described, not silently swallowed")

	// The finished message WAS deleted (only the streaming one failed) --
	// this is a partial failure, not an all-or-nothing rollback, matching
	// the pre-existing per-ID loop semantics (each Delete call is
	// independent; there is no transaction across the batch).
	_, getErr := a.Messages.Get(ctx, finished.ID)
	require.Error(t, getErr, "the terminally finished message should have been deleted")

	stillThere, getErr := a.Messages.Get(ctx, streaming.ID)
	require.NoError(t, getErr, "the still-streaming message must not have been deleted")
	require.Equal(t, "streaming one", stillThere.FullText())
}

// TestHandleDeleteMessages_AllSucceedRepliesOk is the control: when every
// requested ID deletes cleanly, the handler must still reply {"status":
// "ok"} exactly as before -- the fix must not turn every bulk delete into
// an error.
func TestHandleDeleteMessages_AllSucceedRepliesOk(t *testing.T) {
	a, sessionID := newMessageEditTestApp(t)
	ctx := context.Background()

	var ids []string
	for i := 0; i < 2; i++ {
		m, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: "user msg"}},
		})
		require.NoError(t, err)
		ids = append(ids, m.ID)
	}

	c := &Client{send: make(chan []byte, 4)}
	payload, err := json.Marshal(DeleteMessagesPayload{MessageIDs: ids})
	require.NoError(t, err)

	handleDeleteMessages(ctx, a, c, WSMessage{ID: "req-1", Type: CmdDeleteMessages, Payload: payload})

	env := decodeReply(t, c)
	require.Equal(t, EventResponse, env.Type, "bulk delete of all-deletable IDs must still report success")

	for _, id := range ids {
		_, getErr := a.Messages.Get(ctx, id)
		require.Error(t, getErr, "message %s should have been deleted", id)
	}
}

// TestHandleRerunMessage_OrphanedStreamingMessageIsForceDeleted tests that
// rerun over a session whose tail contains an UNFINISHED assistant message
// (an orphan from a crashed/killed turn) does not leave it behind. The tail
// cleanup loop must call ForceDelete when Delete returns ErrMessageStillStreaming,
// since the session has been cancelled and waited for idle, so the orphan will
// never receive a terminal Finish.
//
// This test creates a session with: user message -> assistant message (UNFINISHED).
// It then calls handleRerunMessage on the user message. The orphaned assistant
// message must be force-deleted, and the rerun must proceed without error.
//
// Revert-check: revert the ForceDelete branch in handlers_agent.go's tail
// cleanup loop (remove the errors.Is(delErr, message.ErrMessageStillStreaming)
// check and the ForceDelete call, leaving only the original slog.Warn) and this
// test fails: the orphaned message remains in the session, corrupting the
// transcript by including partial text in LLM context forever.
func TestHandleRerunMessage_OrphanedStreamingMessageIsForceDeleted(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)

	ctx := t.Context()

	// Create a session.
	sess, err := a.Sessions.Create(ctx, "test-rerun-orphan")
	require.NoError(t, err)

	// Create a user message to rerun.
	userMsg, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "original user input"}},
	})
	require.NoError(t, err)

	// Create an assistant message with NO Finish part — this models an orphan
	// left behind by a crashed or killed agent turn.
	assistantMsg, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "orphaned partial response"}},
	})
	require.NoError(t, err)
	require.False(t, assistantMsg.IsFinished(), "precondition: assistant message is not finished")

	// Verify the session has 2 messages.
	messages, err := a.Messages.List(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, messages, 2, "session must have 2 messages before rerun")

	// Mock AgentCoordinator to satisfy the nil check and return idle.
	// We only care about the tail cleanup behavior, not the full rerun path.
	mockCoord := &mockAgentCoordinator{
		busy: false,
	}
	a.AgentCoordinator = mockCoord

	// Create a client to receive the response.
	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)

	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	// Call handleRerunMessage — this should force-delete the orphaned assistant.
	// The function will eventually fail when trying to actually run the agent
	// (we didn't set up a full mock), but the tail cleanup should have succeeded.
	handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})

	// The key assertion: the orphaned assistant message must be gone.
	_, getErr := a.Messages.Get(ctx, assistantMsg.ID)
	require.Error(t, getErr, "orphaned assistant message must be force-deleted")

	// The user message should also be deleted (step 3 of handleRerunMessage).
	_, getErr = a.Messages.Get(ctx, userMsg.ID)
	require.Error(t, getErr, "user message must be deleted (to be recreated by Run)")

	// Verify the session is now empty (or has only the recreated message if
	// the run proceeded further than we care about).
	messagesAfter, err := a.Messages.List(ctx, sess.ID)
	require.NoError(t, err)
	// The session should have 0 messages at this point (the original user and
	// orphaned assistant are gone, and Run hasn't recreated the user message yet
	// because we hit an error further down the path).
	require.Empty(t, messagesAfter, "orphaned message must be removed from the session")
}

// mockAgentCoordinator is a minimal mock for testing handleRerunMessage's
// tail cleanup without needing the full agent coordinator setup.
type mockAgentCoordinator struct {
	busy bool
}

func (m *mockAgentCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, fmt.Errorf("mock coordinator: Run not implemented")
}

func (m *mockAgentCoordinator) RunWithOverrides(ctx context.Context, sessionID, prompt string, smart, fast *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, fmt.Errorf("mock coordinator: RunWithOverrides not implemented")
}

func (m *mockAgentCoordinator) Cancel(sessionID string) {}

func (m *mockAgentCoordinator) CancelAll() (stillBusy bool) {
	return m.busy
}

func (m *mockAgentCoordinator) IsSessionBusy(sessionID string) bool {
	return m.busy
}

func (m *mockAgentCoordinator) IsBusy() bool {
	return m.busy
}

// ReserveExclusive mirrors IsSessionBusy: succeeds (epoch 1) when the mock is
// configured idle, fails when configured busy — consistent with the atomic
// mbIdle->mbOwned contract ReserveExclusive documents (a real mailbox would
// never let a reservation succeed while busy). The tests using this mock
// (TestHandleRerunMessage_OrphanedStreamingMessageIsForceDeleted) configure
// busy: false, so this returns ok=true and lets handleRerunMessage's tail
// cleanup proceed exactly as before task #614's reservation was added.
func (m *mockAgentCoordinator) ReserveExclusive(ctx context.Context, sessionID string) (holdCtx context.Context, epoch uint64, cancel context.CancelFunc, ok bool) {
	if m.busy {
		return nil, 0, nil, false
	}
	return ctx, 1, func() {}, true
}

func (m *mockAgentCoordinator) ReleaseExclusive(sessionID string, epoch uint64, cancel context.CancelFunc) {
}

// RunWithReservedOwnership mirrors Run: unimplemented in this minimal mock.
// handleRerunMessage calls this (not Run/RunWithOverrides) for the
// replacement turn once task #614's reservation hand-off lands, so this must
// return an error the same way Run/RunWithOverrides already do, for the same
// reason the existing orphan test's comment already documents: "the function
// will eventually fail when trying to actually run the agent... but the tail
// cleanup should have succeeded."
func (m *mockAgentCoordinator) RunWithReservedOwnership(ctx context.Context, sessionID, prompt string, epoch uint64, cancel context.CancelFunc, onHandoff func(), smart, fast *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, fmt.Errorf("mock coordinator: RunWithReservedOwnership not implemented")
}

func (m *mockAgentCoordinator) QueuedPrompts(sessionID string) int {
	return 0
}

func (m *mockAgentCoordinator) QueuedPromptsList(sessionID string) []string {
	return nil
}

func (m *mockAgentCoordinator) ClearQueue(sessionID string) {}

func (m *mockAgentCoordinator) InterruptAndSend(ctx context.Context, sessionID, prompt string, smart, fast *agent.ModelOverride, attachments ...message.Attachment) error {
	return nil
}

func (m *mockAgentCoordinator) InjectMessage(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (message.Message, error) {
	return message.Message{}, nil
}

func (m *mockAgentCoordinator) Summarize(context.Context, string, *agent.SummarizeSnapshot) error {
	return nil
}

func (m *mockAgentCoordinator) SummarizeQueued(sessionID string) bool {
	return false
}

func (m *mockAgentCoordinator) TakeSummarizeQueue(sessionID string) (*agent.SummarizeSnapshot, bool) {
	return nil, false
}

func (m *mockAgentCoordinator) RebuildSessionAgentCall(ctx context.Context, data session.SessionAgentCallData) (agent.SessionAgentCall, error) {
	return agent.SessionAgentCall{}, nil
}

func (m *mockAgentCoordinator) RunSessionAgentCall(ctx context.Context, call agent.SessionAgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (m *mockAgentCoordinator) CancelQueuedSummarize(sessionID string) {}

func (m *mockAgentCoordinator) Model() agent.Model {
	return agent.Model{}
}

func (m *mockAgentCoordinator) UpdateModels(ctx context.Context) error {
	return nil
}

func (m *mockAgentCoordinator) GetSystemPrompt() string {
	return ""
}

func (m *mockAgentCoordinator) BuildSystemPrompt(ctx context.Context) (string, error) {
	return "", nil
}

func (m *mockAgentCoordinator) BuildSystemPromptForSession(ctx context.Context, sessionID string) (string, error) {
	return "", nil
}

func (m *mockAgentCoordinator) UpdateSessionSystemPrompt(ctx context.Context, sessionID, prompt string) error {
	return nil
}

func (m *mockAgentCoordinator) SetAgentTimeoutOptions(extendsOnProgress bool, hardCap time.Duration) {
}

func (m *mockAgentCoordinator) SetRunLimits(maxCost float64, maxTokens int64) {}

func (m *mockAgentCoordinator) SetActiveModelRole(role config.SelectedModelType) {}

func (m *mockAgentCoordinator) SetAllowPeakHours(allow bool) {}

func (m *mockAgentCoordinator) SetPersistentMode(persistent bool) {}

func (m *mockAgentCoordinator) ResetAutoResumeCounter(sessionID string) {}
