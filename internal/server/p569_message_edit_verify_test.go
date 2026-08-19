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

// TestUpdateMessageAndVerify_RefusesEditToPartialMessage covers task #590
// (P1-2 in docs/reviews/2026-08-19-release-readiness-static-follow-up-0635c631.md).
//
// The previous fix (task #569 / commit 0cd26cb2) made updateMessageAndVerify
// re-read the row after a write to a mid-stream message and report failure
// if the checkpoint fence was lost. That proves the write landed AT THAT
// INSTANT, but it does not make the edit durable: the agent turn that owns
// the row keeps its own in-memory currentAssistant and, independently of
// this handler, will write it back on the next checkpoint tick or --
// unconditionally, no fence at all -- on the terminal write at the end of
// the turn (see agent_turn.go's OnStepFinish -> a.messages.Update with a
// non-Partial Finish, which takes message.Service.Update's unconditional
// "terminal update always wins" branch). A same-instant read-back cannot
// see that coming.
//
// So the handler must refuse to write at all while the message is still
// mid-stream (Role == Assistant && !IsFinished()), rather than write-and-
// verify only for the moment right after the write.
//
// Revert-check: replace the `if current.Role == message.Assistant &&
// !current.IsFinished() { ... }` guard with the old write-then-verify body
// and this test fails, because the write succeeds and the read-back matches
// (nothing else raced it inside the test) -- but a real turn would still
// clobber it moments later outside the test's control, which is exactly
// the gap this test exists to close at the handler boundary.
func TestUpdateMessageAndVerify_RefusesEditToPartialMessage(t *testing.T) {
	a, sessionID := newMessageEditTestApp(t)
	ctx := context.Background()

	created, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "start"}},
	})
	require.NoError(t, err)

	// A genuine mid-stream checkpoint, written the way the ticker writes it:
	// a Finish part with Partial=true, finished_at stays NULL.
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

	// A WS handler Gets the still-partial row (as handleUpdateMessageContent
	// does) and tries to apply an operator edit to it.
	edit := checkpoint.Clone()
	edit.Parts = []message.ContentPart{message.TextContent{Text: "OPERATOR EDIT"}, edit.Parts[len(edit.Parts)-1]}

	c := &Client{send: make(chan []byte, 4)}
	ok := updateMessageAndVerify(ctx, a, c, "req-1", edit)
	require.False(t, ok, "updateMessageAndVerify must refuse an edit to a message that is still mid-stream")

	select {
	case raw := <-c.send:
		var env WSMessage
		require.NoError(t, json.Unmarshal(raw, &env))
		require.Equal(t, EventError, env.Type,
			"client was told the edit succeeded (or got no reply) for a message that is still streaming")
	default:
		t.Fatal("expected an error reply on the client's send channel")
	}

	// The row must be untouched: the refusal happens before any write.
	stored, err := a.Messages.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, "streamed so far", stored.FullText(),
		"row must be unchanged: the handler must refuse to write, not write-then-detect")
}

// TestUpdateMessageAndVerify_NormalEditLands is the control: a terminally
// finished assistant message takes Update's unconditional path (nothing is
// streaming it anymore, so nothing can race the edit), and
// updateMessageAndVerify must report success.
func TestUpdateMessageAndVerify_NormalEditLands(t *testing.T) {
	a, sessionID := newMessageEditTestApp(t)
	ctx := context.Background()

	created, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "start"}},
	})
	require.NoError(t, err)
	created.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, created))

	edited := created.Clone()
	edited.Parts[0] = message.TextContent{Text: "edited"}

	c := &Client{send: make(chan []byte, 4)}
	ok := updateMessageAndVerify(ctx, a, c, "req-1", edited)
	require.True(t, ok, "editing a terminally finished assistant message must succeed")

	stored, err := a.Messages.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, "edited", stored.FullText())
}

// TestUpdateMessageAndVerify_UserMessageEditLands guards against the most
// tempting wrong fix for #590: gating the mid-stream refusal on
// !IsFinished() alone instead of Role == Assistant && !IsFinished().
//
// In practice message.Service.Create auto-appends a non-partial Finish{Reason:
// "stop"} part to every non-Assistant row at creation time (see Create in
// internal/message/message.go), so plain user messages happen to already be
// IsFinished() == true and would pass a bare !IsFinished() check too. This
// test's real job is guarding the Role gate itself, which is what actually
// matters for any current or future non-Assistant row that is NOT
// synthetically finished (e.g. a differently-constructed row, or if Create's
// auto-Finish behavior ever changes) -- IsFinished() alone is a coincidence
// of today's Create, not a substitute for checking Role == Assistant.
func TestUpdateMessageAndVerify_UserMessageEditLands(t *testing.T) {
	a, sessionID := newMessageEditTestApp(t)
	ctx := context.Background()

	created, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)
	require.Equal(t, message.User, created.Role, "precondition: this is a user message")

	edited := created.Clone()
	edited.Parts[0] = message.TextContent{Text: "hello, edited"}

	c := &Client{send: make(chan []byte, 4)}
	ok := updateMessageAndVerify(ctx, a, c, "req-1", edited)
	require.True(t, ok, "editing a user message must succeed even though IsFinished() is always false for it")

	stored, err := a.Messages.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, "hello, edited", stored.FullText())
}
