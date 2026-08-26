package app

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecoverInterruptedTurns_StaleCandidateNotStamped is the task #777
// (P1 release blocker) proof.
//
// recoverSessionInterruptedTurn re-checks IsFinished(), the liveness lock,
// and the age filter before stamping a candidate -- but none of those
// checks close the gap between the candidate's Get and the eventual write.
// If a NEWER assistant message lands in that exact window (the live owner
// started a fresh turn immediately after this process's discovery query
// ran), the old plain Update would still stamp the now-superseded message
// with "Process restarted", because Update rewrites unconditionally by id.
//
// This test uses the blocking recoverSessionPreWriteSeam (not the existing
// fire-and-forget recoverSessionListSeam, which cannot force a specific
// interleaving and would make this test vacuous) to deterministically land
// a newer assistant message in that exact gap, then asserts the original
// orphan is left untouched and the write is skipped rather than blindly
// retried.
func TestRecoverInterruptedTurns_StaleCandidateNotStamped(t *testing.T) {
	app := newRecoveryTestApp(t)
	ctx := t.Context()

	sess, err := app.Sessions.Create(ctx, "toctou-session")
	require.NoError(t, err)

	orphan, err := app.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "working..."},
			message.ToolCall{ID: "call_1", Name: "bash", Input: `{"command":"echo hi"}`, Finished: true},
		},
	})
	require.NoError(t, err)
	require.False(t, orphan.IsFinished(), "precondition: orphan must not be finished")

	guardsPassed := make(chan struct{})
	proceed := make(chan struct{})
	var newer message.Message

	recoverSessionPreWriteSeam = func() {
		close(guardsPassed)
		<-proceed
	}
	t.Cleanup(func() { recoverSessionPreWriteSeam = nil })

	done := make(chan struct{})
	go func() {
		defer close(done)
		app.recoverInterruptedTurns(ctx)
	}()

	// Wait until recovery has passed every guard for the orphan candidate and
	// is about to issue the stamp write, then land a NEW assistant message in
	// the session -- simulating the live owner starting (and finishing) a
	// fresh turn in the exact TOCTOU window this task closes.
	<-guardsPassed
	newer, err = app.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "actually I'm done now"},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	})
	require.NoError(t, err)
	close(proceed)
	<-done

	// The stale orphan must NOT have been stamped: still unfinished, no
	// FinishReasonError, Parts untouched.
	gotOrphan, err := app.Messages.Get(ctx, orphan.ID)
	require.NoError(t, err)
	assert.False(t, gotOrphan.IsFinished(),
		"stale candidate must be left unfinished, not retroactively stamped 'Process restarted'")
	assert.Nil(t, gotOrphan.FinishPart(),
		"stale candidate must not gain a finish part it didn't have before recovery ran")

	// The newer message (the session's actual last message) must be
	// untouched by recovery.
	gotNewer, err := app.Messages.Get(ctx, newer.ID)
	require.NoError(t, err)
	assert.True(t, gotNewer.IsFinished())
	assert.Equal(t, message.FinishReasonEndTurn, gotNewer.FinishReason(),
		"the session's real last message must keep its own finish reason, not be clobbered")
}
