package server

import (
	"context"
	"encoding/json"
	"testing"

	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

// newMessageEditTestApp builds a minimal *appPkg.App backed by a real SQLite
// connection, with only the Messages field populated. updateMessageAndVerify
// (handlers_messages.go) touches nothing else on App, so this avoids the
// full config/agent-coordinator bootstrap newAttachmentsTestApp uses for
// unrelated tests in this package. Also returns a real session ID: messages
// has a foreign key on session_id, so message.Service.Create needs an
// actual sessions row to attach to.
func newMessageEditTestApp(t *testing.T) (*appPkg.App, string) {
	t.Helper()
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Create(t.Context(), "Test Session")
	require.NoError(t, err)
	return &appPkg.App{Messages: message.NewService(q)}, sess.ID
}

// TestUpdateMessageAndVerify_DetectsLostCheckpointFence covers task #569's
// second half.
//
// message.Service.Update returns nil when a partial-checkpoint write loses
// its conditional fence (0 rows affected) -- that is a pinned, deliberate
// contract for the auto-checkpoint ticker (see
// TestCheckpointFencing_P0_4Regression in
// internal/agent/agent_checkpoint_test.go), not a bug: the ticker cannot
// treat "a real finish already won" as an error without breaking a P0
// guarantee that predates this task.
//
// But the four WS message-editing handlers are not the ticker. They call
// Update on a message loaded via Get, expecting their own edit to land, and
// nil from Update used to be reported to the client as {"status":"ok"} even
// when the row never changed (release-blocker F1, reviewer-confirmed
// probe). updateMessageAndVerify closes that gap on the handler side,
// without touching Update's contract: when the message being written was
// mid-stream (IsPartial()), it re-reads the row after Update and reports an
// error unless the stored parts actually match what was just sent.
//
// This test reproduces the fenced-loss scenario directly: write a genuine
// mid-stream checkpoint row, then a genuine terminal finish (exactly like
// the ticker followed by OnStepFinish), then call updateMessageAndVerify
// with a stale in-memory copy of the pre-terminal partial message -- the
// same shape a WS handler would have if it Got the message a moment before
// the terminal finish landed. Update returns nil (fence lost, pinned
// contract), and updateMessageAndVerify must still report failure to the
// client instead of "ok".
//
// Revert-check: replace the body of updateMessageAndVerify with a bare
// `return a.Messages.Update(...) == nil` (i.e. drop the post-write Get and
// Parts comparison) and this fails -- the client is told "ok" even though
// the row still holds the terminal text, not the stale edit.
func TestUpdateMessageAndVerify_DetectsLostCheckpointFence(t *testing.T) {
	a, sessionID := newMessageEditTestApp(t)
	ctx := context.Background()

	created, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "start"}},
	})
	require.NoError(t, err)

	// A genuine mid-stream checkpoint, written the way the ticker writes it.
	checkpoint := created.Clone()
	checkpoint.Parts = []message.ContentPart{message.TextContent{Text: "streamed so far"}}
	checkpoint.CheckpointGeneration = 1
	checkpoint.AddFinish(message.FinishReasonUnknown, "", "")
	for i := len(checkpoint.Parts) - 1; i >= 0; i-- {
		if f, ok := checkpoint.Parts[i].(message.Finish); ok {
			f.Partial = true
			checkpoint.Parts[i] = f
			break
		}
	}
	require.NoError(t, a.Messages.Update(ctx, checkpoint))

	// A genuine terminal finish lands next (simulating OnStepFinish), racing
	// ahead of a WS handler that already Got the pre-terminal partial state.
	terminal := created.Clone()
	terminal.Parts = []message.ContentPart{message.TextContent{Text: "terminal state"}}
	terminal.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, terminal))

	// The WS handler's view: it Got the message BEFORE the terminal write
	// (so it still has the partial Finish and generation 1), then applied an
	// operator edit to the text, exactly like handleUpdateMessageContent.
	staleView := checkpoint.Clone()
	staleView.Parts = []message.ContentPart{message.TextContent{Text: "OPERATOR EDIT"}, staleView.Parts[len(staleView.Parts)-1]}

	c := &Client{send: make(chan []byte, 4)}
	ok := updateMessageAndVerify(ctx, a, c, "req-1", staleView)
	require.False(t, ok, "updateMessageAndVerify must report failure when the write lost the checkpoint fence")

	// The client must have received an error reply, not {"status":"ok"}.
	select {
	case raw := <-c.send:
		var env WSMessage
		require.NoError(t, json.Unmarshal(raw, &env))
		require.Equal(t, EventError, env.Type,
			"client was told the edit succeeded even though the row still holds the terminal text, not the operator's edit")
	default:
		t.Fatal("expected an error reply on the client's send channel")
	}

	// And the row itself must still hold the terminal text, confirming the
	// edit truly did not land (this is what makes the false "ok" dangerous).
	stored, err := a.Messages.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, "terminal state", stored.FullText(),
		"row should still hold the terminal write; the stale operator edit must not have landed")
}

// TestUpdateMessageAndVerify_NormalEditLands is the control: a non-streaming
// message (no Partial Finish) takes Update's unconditional path, and
// updateMessageAndVerify must report success without needing (or paying
// for) the extra re-read.
func TestUpdateMessageAndVerify_NormalEditLands(t *testing.T) {
	a, sessionID := newMessageEditTestApp(t)
	ctx := context.Background()

	created, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "start"}},
	})
	require.NoError(t, err)

	edited := created.Clone()
	edited.Parts = []message.ContentPart{message.TextContent{Text: "edited"}}

	c := &Client{send: make(chan []byte, 4)}
	ok := updateMessageAndVerify(ctx, a, c, "req-1", edited)
	require.True(t, ok, "a normal (non-streaming) edit must succeed")

	stored, err := a.Messages.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, "edited", stored.FullText())
}
