package server

// Task #651 (first review, finding P2-1): rerun prompt recreation
// on success path should not create duplicate prompts.
//
// The defer registered at line 844 of handleRerunMessage calls
// recreateRerunPromptIfLost unconditionally on every exit path. That helper
// checks len(allMsgs) <= targetIdx and recreates the prompt. This check is
// broken in two ways.
//
// Finding P2-1 (this file's first test): when the replacement turn succeeds
// and has already created the new user prompt, the unconditional defer runs
// anyway and recreates it a second time, leaving the session with duplicate
// prompts. The pure row-count check cannot distinguish "prompt was lost,
// must recreate" from "prompt was successfully created, do nothing" — both
// result in len(allMsgs) > targetIdx. The defer should only recreate on
// error paths, not on success.
//
// Finding P3-1 (this file's second test): when a concurrent unrelated writer
// adds a message after the target is deleted but before the defer's list
// runs, the row count becomes > targetIdx even though the replacement turn
// never created the prompt. The pure row-count check falsely assumes "prompt
// exists, skip recreate", and the operator's prompt is permanently lost.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// Compile-time guarantee that the fake satisfies agent.Coordinator.
var _ agent.Coordinator = (*compactingSuccessCoordinator)(nil)
var _ agent.Coordinator = (*concurrentWriterFailureCoordinator)(nil)

// compactingSuccessCoordinator simulates a successful replacement turn that
// performs in-turn silent compaction: it creates the new prompt, assistant
// reply, and a summary, then deletes old summarized rows such that the final
// row count drops back to <= targetIdx. This exposes P2-1: the unconditional
// defer at line 844 fires on success and creates a duplicate prompt.
type compactingSuccessCoordinator struct {
	cancellableHoldCoordinator
	a *appPkg.App // Stored so RunWithReservedOwnership can use it to create/delete messages.
}

func (m *compactingSuccessCoordinator) RunWithReservedOwnership(ctx context.Context, sessionID, prompt string, epoch uint64, cancel context.CancelFunc, onHandoff func(), smart, fast *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	// Signal that the handoff point has been reached (disarms the
	// handler's releaseOnBailout defer).
	if onHandoff != nil {
		onHandoff()
	}

	// Simulate the replacement turn's createUserMessage: create the new
	// user prompt row with the captured text.
	_, err := m.a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: prompt}},
	})
	if err != nil {
		return nil, err
	}

	// Simulate the rest of the turn: create an assistant reply.
	reply, err := m.a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "new reply"}},
	})
	if err != nil {
		return nil, err
	}
	reply.AddFinish(message.FinishReasonEndTurn, "", "")
	if err := m.a.Messages.Update(ctx, reply); err != nil {
		return nil, err
	}

	// Simulate in-turn silent compaction: create a summary user row.
	summary, err := m.a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "summary of earlier conversation"}},
	})
	if err != nil {
		return nil, err
	}

	// Simulate compaction: delete pre-existing old rows.
	// After step 3 of the handler, the remaining messages are user0 and assistant0
	// (the messages before the rerun target). The fake has created 3 new messages
	// (prompt, reply, summary), so total is 5. We need to delete 3 to get back to 2.
	allMsgs, err := m.a.Messages.List(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	// Find and delete the old messages by their text content.
	for _, msg := range allMsgs {
		if msg.Role == message.User && msg.Content().Text == "earlier question" {
			if err := m.a.Messages.Delete(ctx, msg.ID); err != nil {
				// Ignore errors.
			}
		}
		if msg.Role == message.Assistant && msg.Content().Text == "earlier answer" {
			if err := m.a.Messages.Delete(ctx, msg.ID); err != nil {
				// Ignore errors.
			}
		}
	}

	// Delete the summary to bring the final count to exactly 2.
	if err := m.a.Messages.Delete(ctx, summary.ID); err != nil {
		// Ignore errors.
	}

	// Release ownership and return success.
	m.ReleaseExclusive(sessionID, epoch, cancel)
	return nil, nil
}

// TestHandleRerunMessage_SuccessWithCompactionDoesNotDuplicatePrompt is
// the regression test for task #651 finding P2-1: when the replacement
// turn succeeds (and has already created the new user prompt), the
// unconditional defer at line 844 must NOT recreate it a second time.
// The test sets up a fake that simulates in-turn silent compaction which
// deletes enough old rows that len(allMsgs) <= targetIdx, triggering the
// bug where the defer blindly recreates the prompt on the success path.
//
// History setup (targetIdx=2 after tail delete):
//
//	user0: "earlier question"
//	assistant0: "earlier answer" (finished)
//	userMsg: "rerun me" (rerun target)
//	assistant1: "old reply" (finished, deleted by tail delete)
//
// The fake creates:
//
//	user2: "rerun me" (new prompt)
//	assistant2: "new reply" (finished)
//	user3: "summary" (deleted in compaction)
//
// After compaction, final state is [user2, assistant2] (2 messages = targetIdx).
//
// Revert-check: the current unconditional defer fires on success and creates
// a duplicate when len(allMsgs) <= targetIdx, so this test fails with
// count=2 instead of count=1.
func TestHandleRerunMessage_SuccessWithCompactionDoesNotDuplicatePrompt(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-651-compaction")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	// Create the history before the rerun target.
	_, err = a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier question"}},
	})
	require.NoError(t, err)

	assistant0, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier answer"}},
	})
	require.NoError(t, err)
	assistant0.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, assistant0))

	// Create the rerun target.
	userMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)

	// Create the tail (will be deleted by step 2's tail delete).
	tailMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "old reply"}},
	})
	require.NoError(t, err)
	tailMsg.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, tailMsg))

	// Verify the initial state: 4 messages in total.
	initialMsgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, 4, len(initialMsgs),
		"precondition: 4 messages initially (user0, assistant0, userMsg, tail)")
	// targetIdx should be 2 (user0 at 0, assistant0 at 1, userMsg at 2, tail at 3).
	targetIdx := 2
	for i, m := range initialMsgs {
		if m.ID == userMsg.ID {
			targetIdx = i
			break
		}
	}
	require.Equal(t, 2, targetIdx, "precondition: target message is at index 2")

	// Set up the fake coordinator.
	mockCoord := &compactingSuccessCoordinator{a: a}
	a.AgentCoordinator = mockCoord

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("rerun handler never returned — something blocked unboundedly")
	}

	env := decodeReply(t, client)
	require.Equal(t, EventResponse, env.Type,
		"the fake returns success, so the handler must reply ok")

	// The decisive assertion: exactly ONE user message with text "rerun me".
	// The current buggy code creates a duplicate via the unconditional defer.
	msgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)

	count := 0
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == "rerun me" {
			count++
		}
	}
	require.Equal(t, 1, count,
		"the handler must NOT create a duplicate prompt on the success path; "+
			"the unconditional defer creates a spurious second copy (count=%d, expected 1) — "+
			"this is finding P2-1 of task #651", count)

	// Verify the tail was deleted.
	_, err = a.Messages.Get(ctx, tailMsg.ID)
	require.Error(t, err, "tail should be deleted by the handler's step 2")
}

// concurrentWriterFailureCoordinator simulates a replacement turn that fails
// early (releasing ownership before creating anything) but a concurrent
// unrelated writer adds a message after the target is deleted. This exposes
// P3-1: the pure row-count check in the defer sees len(allMsgs) > targetIdx
// and assumes the prompt exists, so it skips recreate — but the prompt was
// never created by the failed turn, so it's permanently lost.
type concurrentWriterFailureCoordinator struct {
	cancellableHoldCoordinator
	a *appPkg.App
}

func (m *concurrentWriterFailureCoordinator) RunWithReservedOwnership(ctx context.Context, sessionID, prompt string, epoch uint64, cancel context.CancelFunc, onHandoff func(), smart, fast *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	// Release ownership FIRST (mirroring early-error paths in agent_run.go).
	m.ReleaseExclusive(sessionID, epoch, cancel)

	// Simulate a concurrent unrelated writer adding a message after the
	// target is deleted but before the defer's list runs.
	_, err := m.a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "unrelated concurrent message"}},
	})
	if err != nil {
		return nil, err
	}

	// Return an error (the replacement turn failed).
	return nil, errors.New("model resolution failed")
}

// TestHandleRerunMessage_ConcurrentUnrelatedWriterDoesNotSuppressRecreate is
// the regression test for task #651 finding P3-1: when the replacement turn
// fails early (before creating the new prompt) but a concurrent unrelated
// writer adds a message, the defer's pure row-count check must not be fooled
// into skipping recreate. The test sets up a fake that releases ownership
// immediately and simulates a concurrent writer adding an unrelated message,
// which makes len(allMsgs) > targetIdx even though the prompt is lost.
//
// History setup (targetIdx=0 after tail delete):
//
//	userMsg: "rerun me" (rerun target, at index 0)
//	assistant0: "old reply" (finished, deleted by tail delete)
//
// The fake:
//
//	Releases ownership (early error)
//	Adds an unrelated message "unrelated concurrent message"
//	Returns error "model resolution failed"
//
// After the fake, len(allMsgs) = 1 > targetIdx = 0, but the unrelated
// message is NOT the prompt, so the defer MUST recreate the prompt.
//
// Revert-check: the current pure row-count check sees len > targetIdx and
// skips recreate, so this test fails with count=0 instead of count=1.
func TestHandleRerunMessage_ConcurrentUnrelatedWriterDoesNotSuppressRecreate(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-651-concurrent-writer")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	// Create the rerun target (at index 0).
	userMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)

	// Create the tail (will be deleted by step 2's tail delete).
	tailMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "old reply"}},
	})
	require.NoError(t, err)
	tailMsg.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, tailMsg))

	// Verify the initial state: 2 messages in total.
	initialMsgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, 2, len(initialMsgs),
		"precondition: 2 messages initially (userMsg, tail)")
	// targetIdx should be 0 (userMsg at 0, tail at 1).
	targetIdx := 0
	for i, m := range initialMsgs {
		if m.ID == userMsg.ID {
			targetIdx = i
			break
		}
	}
	require.Equal(t, 0, targetIdx, "precondition: target message is at index 0")

	// Set up the fake coordinator.
	mockCoord := &concurrentWriterFailureCoordinator{a: a}
	a.AgentCoordinator = mockCoord

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("rerun handler never returned — something blocked unboundedly")
	}

	env := decodeReply(t, client)
	require.Equal(t, EventError, env.Type,
		"the fake returns an error, so the handler must reply with an error")
	require.Contains(t, env.Error, "model resolution failed")

	// The decisive assertion: exactly ONE user message with text "rerun me".
	// The current buggy code sees len(allMsgs) > targetIdx (because of the
	// concurrent writer's unrelated message) and skips recreate, so count=0.
	msgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)

	count := 0
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == "rerun me" {
			count++
		}
	}
	require.Equal(t, 1, count,
		"the handler MUST recreate the prompt even when a concurrent writer adds an unrelated message; "+
			"the pure row-count check is fooled and skips recreate, leaving the prompt lost (count=%d, expected 1) — "+
			"this is finding P3-1 of task #651", count)

	// Verify the tail was deleted.
	_, err = a.Messages.Get(ctx, tailMsg.ID)
	require.Error(t, err, "tail should be deleted by the handler's step 2")

	// Verify the concurrent writer's message exists.
	hasUnrelated := false
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == "unrelated concurrent message" {
			hasUnrelated = true
			break
		}
	}
	require.True(t, hasUnrelated, "the concurrent writer's message should exist")

	// Total messages: recreated "rerun me" + unrelated = 2.
	require.Equal(t, 2, len(msgs),
		"after concurrent writer and prompt recreate, the session should have 2 messages")
}
