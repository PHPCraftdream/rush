package server

// Task #644 (twelfth review, finding N-1): #638's promptExists scan
// matched ANY User row anywhere in the session with the same text — not
// just one the replacement turn could have created. Because the tail
// delete only removes messages AFTER the target, an EARLIER identical
// prompt ("continue", "go on", …) survives and suppresses the recreate,
// silently dropping the turn the operator asked to rerun.
//
// The fix is positional: everything at index >= targetIdx in the original
// list was deleted by steps 2/3, so if the post-failure list still has
// targetIdx+1 or more messages, the extra rows are necessarily new (the
// replacement turn got far enough to persist the prompt) and no recreate
// is needed; otherwise recreate. Text content is irrelevant.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestHandleRerunMessage_PromptRecreatedDespiteEarlierIdenticalPrompt
// reproduces the review's exact scenario: history [U1 "continue", A1,
// U2 "continue", A2], rerun U2, the replacement turn fails before
// createUserMessage. An earlier identical "continue" must NOT suppress
// the recreate.
//
// Revert-check: restore the text-scan promptExists condition and this
// test fails with 1 user message instead of 2.
func TestHandleRerunMessage_PromptRecreatedDespiteEarlierIdenticalPrompt(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-644-prompt-collision")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	for _, m := range []message.CreateMessageParams{
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "continue"}}},
		{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "first reply"}}},
		{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "continue"}}},
		{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "second reply"}}},
	} {
		created, err := a.Messages.Create(ctx, sessionID, m)
		require.NoError(t, err)
		if m.Role == message.Assistant {
			created.AddFinish(message.FinishReasonEndTurn, "", "")
			require.NoError(t, a.Messages.Update(ctx, created))
		}
	}

	msgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, msgs, 4)
	targetID := msgs[2].ID // U2, the second "continue"

	a.AgentCoordinator = &earlyReturnCoordinator{}

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: targetID})
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

	msgs, err = a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	count := 0
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == "continue" {
			count++
		}
	}
	require.Equal(t, 2, count,
		"the rerun target's prompt must be recreated even when an earlier identical prompt survives the tail delete")
	require.Len(t, msgs, 3,
		"final transcript must be [U1, A1, recreated U] — A2 and the original U2 deleted")
}
