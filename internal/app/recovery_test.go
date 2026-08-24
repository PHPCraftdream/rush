package app

import (
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRecoveryTestApp wires up just enough of App to exercise
// recoverInterruptedTurns: real session.Service and message.Service against
// an in-memory SQLite (t.TempDir-backed via db.Connect). Anything not
// needed by recovery (LSP, MCP, agent coordinator, etc.) is left nil.
func newRecoveryTestApp(t *testing.T) *App {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	q := db.New(conn)
	// Zero orphan-age threshold so tests don't need to sleep for the
	// production default (30s) to elapse before recovery considers a
	// freshly-created assistant an orphan. Production safety net for
	// concurrent-process race is exercised separately in
	// TestRecoverInterruptedTurns_RespectsAgeFilter.
	zero := time.Duration(0)
	return &App{
		Sessions:          session.NewService(q, conn),
		Messages:          message.NewService(q),
		recoveryOrphanAge: &zero,
	}
}

func TestRecoverInterruptedTurns_NoSessions_NoOp(t *testing.T) {
	app := newRecoveryTestApp(t)
	// Must not panic and must not error — recovery is best-effort.
	app.recoverInterruptedTurns(t.Context())
}

func TestRecoverInterruptedTurns_SessionWithFinishedAssistant_LeavesItAlone(t *testing.T) {
	app := newRecoveryTestApp(t)
	ctx := t.Context()

	sess, err := app.Sessions.Create(ctx, "test")
	require.NoError(t, err)

	asst, err := app.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "hi there"},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	})
	require.NoError(t, err)
	require.True(t, asst.IsFinished(), "precondition: assistant should be finished")

	app.recoverInterruptedTurns(ctx)

	// Re-read — must be unchanged.
	got, err := app.Messages.Get(ctx, asst.ID)
	require.NoError(t, err)
	assert.Equal(t, message.FinishReasonEndTurn, got.FinishReason(),
		"recovery must not touch already-finished assistant messages")
}

func TestRecoverInterruptedTurns_OrphanAssistant_GetsErrorFinish(t *testing.T) {
	app := newRecoveryTestApp(t)
	ctx := t.Context()

	sess, err := app.Sessions.Create(ctx, "stuck-session")
	require.NoError(t, err)

	// Simulate the 162-promise-all symptom: an assistant message with a
	// tool call but NO finish part. This is what a process death
	// mid-stream leaves behind.
	orphan, err := app.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "I'll start by..."},
			message.ToolCall{
				ID:       "call_orphan_1",
				Name:     "bash",
				Input:    `{"command":"echo hi"}`,
				Finished: true,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, orphan.IsFinished(), "precondition: orphan must NOT have finish part")

	app.recoverInterruptedTurns(ctx)

	got, err := app.Messages.Get(ctx, orphan.ID)
	require.NoError(t, err)
	assert.True(t, got.IsFinished(), "recovery must add a finish part")
	assert.Equal(t, message.FinishReasonError, got.FinishReason(),
		"orphan recovery uses FinishReasonError so the UI can show it as an interrupted turn")

	fp := got.FinishPart()
	require.NotNil(t, fp)
	assert.Equal(t, "Process restarted", fp.Message,
		"the brief, user-visible title")
	// The details should mention silent-dying / restart so a debugging
	// user can correlate with the CHANGELOG.
	assert.Contains(t, strings.ToLower(fp.Details), "process",
		"details should explain what happened")
}

func TestRecoverInterruptedTurns_SessionWithUserMessageOnly_LeavesItAlone(t *testing.T) {
	app := newRecoveryTestApp(t)
	ctx := t.Context()

	sess, err := app.Sessions.Create(ctx, "user-only")
	require.NoError(t, err)

	// User message exists, but no assistant message yet. This is the
	// "user sent prompt, agent didn't start" state — nothing to recover.
	_, err = app.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{
			message.TextContent{Text: "hello"},
		},
	})
	require.NoError(t, err)

	// Must be a no-op.
	app.recoverInterruptedTurns(ctx)

	msgs, err := app.Messages.List(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, message.User, msgs[0].Role)
}

// TestRecoverInterruptedTurns_RespectsAgeFilter verifies the
// concurrent-process safety net: a freshly-created (just-now) orphan
// assistant must NOT be marked as restarted, because it might be a
// live in-progress message from a parallel crush process. We exercise
// this by NOT zeroing the threshold (overriding the default 30s).
func TestRecoverInterruptedTurns_RespectsAgeFilter(t *testing.T) {
	app := newRecoveryTestApp(t)
	// Restore production threshold for this test.
	defaultThreshold := 30 * time.Second
	app.recoveryOrphanAge = &defaultThreshold
	ctx := t.Context()

	sess, err := app.Sessions.Create(ctx, "fresh-orphan")
	require.NoError(t, err)
	orphan, err := app.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "x", Name: "bash", Input: "{}", Finished: true},
		},
	})
	require.NoError(t, err)
	require.False(t, orphan.IsFinished())

	app.recoverInterruptedTurns(ctx)

	got, err := app.Messages.Get(ctx, orphan.ID)
	require.NoError(t, err)
	assert.False(t, got.IsFinished(),
		"recovery must skip recently-created assistants — could be a fresh message from a parallel crush process")
}

// TestRecoverInterruptedTurns_MultipleSessions_OnlyOrphansTouched mirrors
// the realistic shape of D:\dev\garnet-team\.crush at startup time: many
// sessions, only a few have orphan assistants.
func TestRecoverInterruptedTurns_MultipleSessions_OnlyOrphansTouched(t *testing.T) {
	app := newRecoveryTestApp(t)
	ctx := t.Context()

	// 3 sessions: 1 healthy (assistant finished), 1 orphan, 1 user-only.
	healthy, err := app.Sessions.Create(ctx, "healthy")
	require.NoError(t, err)
	healthyAsst, err := app.Messages.Create(ctx, healthy.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "done"},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	})
	require.NoError(t, err)

	orphan, err := app.Sessions.Create(ctx, "orphan")
	require.NoError(t, err)
	orphanAsst, err := app.Messages.Create(ctx, orphan.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "x", Name: "grep", Input: "{}", Finished: true},
		},
	})
	require.NoError(t, err)
	require.False(t, orphanAsst.IsFinished())

	userOnly, err := app.Sessions.Create(ctx, "user-only")
	require.NoError(t, err)
	_, err = app.Messages.Create(ctx, userOnly.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hi"}},
	})
	require.NoError(t, err)

	app.recoverInterruptedTurns(ctx)

	// Healthy assistant unchanged.
	gotHealthy, err := app.Messages.Get(ctx, healthyAsst.ID)
	require.NoError(t, err)
	assert.Equal(t, message.FinishReasonEndTurn, gotHealthy.FinishReason())

	// Orphan now has error finish.
	gotOrphan, err := app.Messages.Get(ctx, orphanAsst.ID)
	require.NoError(t, err)
	assert.Equal(t, message.FinishReasonError, gotOrphan.FinishReason())

	// User-only session untouched.
	userMsgs, err := app.Messages.List(ctx, userOnly.ID)
	require.NoError(t, err)
	require.Len(t, userMsgs, 1)
}

// --- Piece 4: findOrphanPartial (batch 8, crash-resilience) ---

func TestFindOrphanPartial_WithPartialOrphan(t *testing.T) {
	app := newRecoveryTestApp(t)
	ctx := t.Context()

	sess, err := app.Sessions.Create(ctx, "partial-session")
	require.NoError(t, err)

	// Create a user message so there's history before the partial assistant.
	_, err = app.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "do the thing"}},
	})
	require.NoError(t, err)

	// Simulate an assistant that was mid-stream when SIGTERM hit:
	// text content + a Finish with Partial=true (set by the checkpoint goroutine).
	asst, err := app.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "Here is the answer you asked for: the result is 42"},
			message.Finish{Reason: message.FinishReasonEndTurn, Partial: true},
		},
	})
	require.NoError(t, err)
	require.True(t, asst.IsPartial(), "precondition: must be partial")
	require.False(t, asst.IsFinished(), "precondition: partial finish must NOT count as finished")

	got := app.findOrphanPartial(ctx, sess.ID)
	require.NotNil(t, got, "findOrphanPartial must find the partial orphan")
	assert.Equal(t, asst.ID, got.MessageID)
	assert.Equal(t, len("Here is the answer you asked for: the result is 42"), got.Chars)
	assert.Contains(t, got.Text, "the result is 42")
}

func TestFindOrphanPartial_NoOrphan(t *testing.T) {
	app := newRecoveryTestApp(t)
	ctx := t.Context()

	sess, err := app.Sessions.Create(ctx, "clean-session")
	require.NoError(t, err)

	// Empty session — no messages at all.
	got := app.findOrphanPartial(ctx, sess.ID)
	assert.Nil(t, got, "empty session must return nil")

	// Session with only a finished assistant — also no orphan.
	_, err = app.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "all done"},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	})
	require.NoError(t, err)

	got = app.findOrphanPartial(ctx, sess.ID)
	assert.Nil(t, got, "finished assistant must not be treated as orphan")
}

func TestFindOrphanPartial_OnlyLatestOrphan(t *testing.T) {
	app := newRecoveryTestApp(t)
	ctx := t.Context()

	sess, err := app.Sessions.Create(ctx, "multi-partial")
	require.NoError(t, err)

	// Older partial assistant (simulating a previous interrupted turn).
	older, err := app.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "old partial text"},
			message.Finish{Reason: message.FinishReasonEndTurn, Partial: true},
		},
	})
	require.NoError(t, err)
	require.True(t, older.IsPartial())

	// Newer partial assistant (the most recent interrupted turn).
	newer, err := app.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "new partial text that is longer"},
			message.Finish{Reason: message.FinishReasonEndTurn, Partial: true},
		},
	})
	require.NoError(t, err)
	require.True(t, newer.IsPartial())

	got := app.findOrphanPartial(ctx, sess.ID)
	require.NotNil(t, got, "must find the latest orphan")
	assert.Equal(t, newer.ID, got.MessageID,
		"must surface the NEWEST partial, not the oldest")
	assert.Contains(t, got.Text, "new partial text")
	assert.NotEqual(t, older.ID, got.MessageID,
		"must NOT surface the older orphan")
}

// TestRecoverInterruptedTurns_ChildSessionOrphan_GetsErrorFinish reproduces
// the known recovery gap: recoverInterruptedTurns only sweeps TOP-LEVEL sessions
// (Sessions.List filters parent_session_id IS NULL), so a sub-agent CHILD
// session's trailing assistant message left mid-stream by a hard kill never gets
// a finish part.
//
// This test builds a parent with a finished assistant (delegating via a tool)
// and a child session with an orphan trailing assistant. After recovery, the
// child's orphan should be marked with FinishReasonError — but this assertion
// FAILS against the current code because child sessions are not swept.
func TestRecoverInterruptedTurns_ChildSessionOrphan_GetsErrorFinish(t *testing.T) {
	app := newRecoveryTestApp(t)
	ctx := t.Context()

	// Create a top-level parent session.
	parent, err := app.Sessions.Create(ctx, "parent-session")
	require.NoError(t, err)

	// In the parent, create an assistant message that is ALREADY finished.
	// This simulates a delegation that completed cleanly before the hard kill.
	parentAsst, err := app.Messages.Create(ctx, parent.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "delegating..."},
			message.Finish{Reason: message.FinishReasonToolUse},
		},
	})
	require.NoError(t, err)
	require.True(t, parentAsst.IsFinished(), "precondition: parent assistant should be finished")
	require.Equal(t, message.FinishReasonToolUse, parentAsst.FinishReason(),
		"precondition: parent assistant should have FinishReasonToolUse")

	// Create a child session via CreateTaskSession. The toolCallID becomes the
	// child session ID, and parent_session_id is set to the parent's ID.
	child, err := app.Sessions.CreateTaskSession(ctx, "call_child_1", parent.ID, "sub-agent task")
	require.NoError(t, err)

	// In the child, create an orphan trailing assistant: text + a tool call
	// but NO finish part. This is what a hard kill during a sub-agent delegation
	// leaves behind.
	childOrphan, err := app.Messages.Create(ctx, child.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "working in child..."},
			message.ToolCall{
				ID:       "call_x",
				Name:     "bash",
				Input:    `{"command":"echo hi"}`,
				Finished: true,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, childOrphan.IsFinished(), "precondition: child orphan must NOT have finish part")

	app.recoverInterruptedTurns(ctx)

	// Re-read the child's orphan message and verify it now has a finish part.
	gotChild, err := app.Messages.Get(ctx, childOrphan.ID)
	require.NoError(t, err)
	assert.True(t, gotChild.IsFinished(), "recovery must add a finish part to the child's orphan assistant")
	assert.Equal(t, message.FinishReasonError, gotChild.FinishReason(),
		"orphan recovery uses FinishReasonError so the UI can show it as an interrupted turn")
	fp := gotChild.FinishPart()
	require.NotNil(t, fp)
	assert.Equal(t, "Process restarted", fp.Message,
		"the brief, user-visible title")

	// The parent's already-finished assistant must not be touched.
	gotParent, err := app.Messages.Get(ctx, parentAsst.ID)
	require.NoError(t, err)
	assert.Equal(t, message.FinishReasonToolUse, gotParent.FinishReason(),
		"recovery must not touch the parent's already-finished assistant message")
}

// TestRecoverInterruptedTurns_ChildSessionAlreadyFinished_LeavesItAlone is the
// companion non-regression test: if a child session's trailing assistant already
// has a finish part (meaning the child completed cleanly), recovery must leave it
// alone. This proves the sweep doesn't blindly add a second finish to every
// child assistant it sees.
func TestRecoverInterruptedTurns_ChildSessionAlreadyFinished_LeavesItAlone(t *testing.T) {
	app := newRecoveryTestApp(t)
	ctx := t.Context()

	// Create a parent + child pair.
	parent, err := app.Sessions.Create(ctx, "parent-finished")
	require.NoError(t, err)

	child, err := app.Sessions.CreateTaskSession(ctx, "call_child_finished", parent.ID, "finished sub-agent")
	require.NoError(t, err)

	// In the child, create a trailing assistant that already has a finish part.
	// This simulates a sub-agent that completed before the hard kill.
	childAsst, err := app.Messages.Create(ctx, child.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "done with child task"},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	})
	require.NoError(t, err)
	require.True(t, childAsst.IsFinished(), "precondition: child assistant should be finished")
	require.Equal(t, message.FinishReasonEndTurn, childAsst.FinishReason(),
		"precondition: child assistant should have FinishReasonEndTurn")

	app.recoverInterruptedTurns(ctx)

	// Re-read the child's assistant and verify its finish reason is unchanged.
	got, err := app.Messages.Get(ctx, childAsst.ID)
	require.NoError(t, err)
	assert.Equal(t, message.FinishReasonEndTurn, got.FinishReason(),
		"recovery must not modify a child session's already-finished assistant")
}
