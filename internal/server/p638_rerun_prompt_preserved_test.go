package server

// Task #638 (eleventh review, finding B-1): preserving the user's prompt
// when the replacement turn fails before recreating it.
//
// Step 3 of handleRerunMessage deletes the target user message; only the
// replacement turn's createUserMessage (agent_prompt.go) recreates it, deep
// inside Run. RunWithReservedOwnership has several early-return error paths
// BEFORE createUserMessage runs (coordinator_run.go's readyWg.Wait /
// applyModelOverrides / resolveSessionModels / buildCall, agent_run.go's
// tryAdmitRunWg / rebindDispatcher / session-lock failures) — reachable in
// normal operation, e.g. a session whose smart_model_id no longer resolves.
// When one fires, the handler used to reply EventError and return, leaving
// the session with zero user messages: the operator's own words gone with
// no target row left to retry from.
//
// The fix: on the error path, scan the session for a User message with the
// captured prompt text; if the run already got far enough to persist one
// (errors past the #339 "attempted" boundary also surface here), leave it
// alone; otherwise recreate it from the captured text before replying.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// Compile-time guarantee that the fake satisfies agent.Coordinator.
var _ agent.Coordinator = (*earlyReturnCoordinator)(nil)

// earlyReturnCoordinator reproduces the real RunWithReservedOwnership
// early-return shape: it releases the reservation itself, NEVER invokes
// onHandoff, and returns an error — exactly what coordinator_run.go /
// agent_run.go do before the turn ever reaches createUserMessage.
type earlyReturnCoordinator struct {
	cancellableHoldCoordinator
	// preCreate, when non-nil, runs BEFORE the failure and persists the
	// prompt as a user message (simulating a run that got past
	// createUserMessage and failed later).
	preCreate func(ctx context.Context, sessionID, prompt string)
}

func (m *earlyReturnCoordinator) RunWithReservedOwnership(ctx context.Context, sessionID, prompt string, epoch uint64, cancel context.CancelFunc, onHandoff func(), smart, fast *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	if m.preCreate != nil {
		m.preCreate(ctx, sessionID, prompt)
	}
	m.ReleaseExclusive(sessionID, epoch, cancel)
	return nil, errors.New("model resolution failed: smart model not found")
}

// TestHandleRerunMessage_PromptPreservedWhenReplacementTurnFailsEarly is
// THE regression test for task #638: a RunWithReservedOwnership that fails
// BEFORE recreating the user message must not leave the operator's prompt
// lost. The handler may (and must) still reply with the error — but the
// session must end with exactly one User message carrying the prompt text.
//
// Revert-check: remove the recreate-on-error-path block in the err != nil
// branch of handleRerunMessage and this test fails with zero user messages
// in the session.
func TestHandleRerunMessage_PromptPreservedWhenReplacementTurnFailsEarly(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-638-early-return")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	userMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)

	tailMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "old reply"}},
	})
	require.NoError(t, err)
	tailMsg.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, tailMsg))

	mockCoord := &earlyReturnCoordinator{}
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
		"the run itself failed; the operator must still see the failure")
	require.Contains(t, env.Error, "model resolution failed")

	// The original row was deleted at step 3; the recreated one has a new ID.
	_, getErr := a.Messages.Get(ctx, userMsg.ID)
	require.Error(t, getErr, "step 3 must still delete the original target row exactly once")

	msgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	count := 0
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == "rerun me" {
			count++
		}
	}
	require.Equal(t, 1, count,
		"the user's prompt must be recreated exactly once when the replacement turn fails before createUserMessage")
}

// TestHandleRerunMessage_NoDuplicateWhenPromptAlreadyRecreated covers the
// other half: when the run got PAST createUserMessage and failed later, the
// prompt row already exists and the handler's restore scan must NOT create
// a second copy.
//
// Revert-check: replace the scan with a blind Create and this test fails
// with two user messages.
func TestHandleRerunMessage_NoDuplicateWhenPromptAlreadyRecreated(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-638-fail-after-create")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	userMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)

	tailMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "old reply"}},
	})
	require.NoError(t, err)
	tailMsg.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, tailMsg))

	mockCoord := &earlyReturnCoordinator{
		preCreate: func(ctx context.Context, sessionID, prompt string) {
			_, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
				Role:  message.User,
				Parts: []message.ContentPart{message.TextContent{Text: prompt}},
			})
			require.NoError(t, err)
		},
	}
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
	require.Equal(t, EventError, env.Type)
	require.Contains(t, env.Error, "model resolution failed")

	msgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	count := 0
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == "rerun me" {
			count++
		}
	}
	require.Equal(t, 1, count,
		"the handler must not duplicate the prompt when the run already persisted it before failing")
}
