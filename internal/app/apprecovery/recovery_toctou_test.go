package apprecovery

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/store/message"
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
	sessions, msgs, params, diagnostics := newRecoveryTestServices(t)
	ctx := t.Context()
	recoveryCtx, cancelRecovery := context.WithCancel(ctx)
	defer cancelRecovery()

	sess, err := sessions.Create(ctx, "toctou-session")
	require.NoError(t, err)

	orphan, err := msgs.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "working..."},
			message.ToolCall{ID: "call_1", Name: "bash", Input: `{"command":"echo hi"}`, Finished: true},
		},
	})
	require.NoError(t, err)
	diagnostics.candidate(sess.ID, orphan.ID)
	diagnostics.eventf("TOCTOU orphan session=%s message=%s", sess.ID, orphan.ID)
	require.False(t, orphan.IsFinished(), "precondition: orphan must not be finished")

	guardsPassed := make(chan struct{})
	proceed := make(chan struct{})
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(proceed) }) }
	var newer message.Message

	priorPreWrite := params.PreWriteSeam
	params.PreWriteSeam = func() {
		if priorPreWrite != nil {
			priorPreWrite()
		}
		diagnostics.eventf("PreWriteSeam reached for candidate message=%s", orphan.ID)
		close(guardsPassed)
		select {
		case <-proceed:
		case <-recoveryCtx.Done():
		}
	}

	const waitTimeout = 5 * time.Second
	done := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(done)
		RecoverInterruptedTurns(recoveryCtx, params)
	}()
	t.Cleanup(func() {
		releaseGate()
		cancelRecovery()
		joinStarted := time.Now()
		joinTarget := "recovery worker completion"
		joinSeen := "completed"
		select {
		case <-done:
		case <-time.After(waitTimeout):
			joinSeen = "cleanup join timeout"
		}
		joinElapsed := time.Since(joinStarted)
		diagnostics.wait(joinTarget, joinSeen, joinElapsed, waitTimeout)
		diagnostics.eventf("cleanup join target=%q seen=%q elapsed=%s timeout=%s", joinTarget, joinSeen, joinElapsed, waitTimeout)
		if joinSeen != "completed" {
			t.Errorf("recovery worker did not join during cleanup: target=%q seen=%q elapsed=%s timeout=%s (worker started %s ago)",
				joinTarget, joinSeen, joinElapsed, waitTimeout, time.Since(started))
		}
	})

	// The seam announces the write boundary before blocking. Keep the newer
	// insert between that signal and releaseGate to preserve the race proof.
	waitStarted := time.Now()
	waitTarget := "PreWriteSeam for " + orphan.ID
	waitSeen := "not reached"
	waitOK := false
	select {
	case <-guardsPassed:
		waitSeen = "PreWriteSeam reached"
		waitOK = true
	case <-time.After(waitTimeout):
		waitSeen = "timeout waiting for PreWriteSeam"
	case <-ctx.Done():
		waitSeen = "test context canceled before PreWriteSeam"
	}
	waitElapsed := time.Since(waitStarted)
	diagnostics.wait(waitTarget, waitSeen, waitElapsed, waitTimeout)
	diagnostics.eventf("wait target=%q seen=%q elapsed=%s timeout=%s", waitTarget, waitSeen, waitElapsed, waitTimeout)
	if !waitOK {
		t.Errorf("recovery did not reach pre-write seam: target=%q seen=%q elapsed=%s timeout=%s", waitTarget, waitSeen, waitElapsed, waitTimeout)
		return
	}

	newer, err = msgs.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "actually I'm done now"},
			message.Finish{Reason: message.FinishReasonEndTurn},
		},
	})
	require.NoError(t, err)
	diagnostics.candidate(sess.ID, newer.ID)
	diagnostics.eventf("newer finished message inserted session=%s message=%s", sess.ID, newer.ID)
	releaseGate()
	doneWaitStarted := time.Now()
	doneWaitTarget := "recovery worker completion"
	doneWaitSeen := "completed"
	select {
	case <-done:
	case <-time.After(waitTimeout):
		doneWaitSeen = "timeout waiting for recovery worker completion"
	}
	doneWaitElapsed := time.Since(doneWaitStarted)
	diagnostics.wait(doneWaitTarget, doneWaitSeen, doneWaitElapsed, waitTimeout)
	diagnostics.eventf("wait target=%q seen=%q elapsed=%s timeout=%s", doneWaitTarget, doneWaitSeen, doneWaitElapsed, waitTimeout)
	if doneWaitSeen != "completed" {
		t.Errorf("recovery worker did not finish after gate release: target=%q seen=%q elapsed=%s timeout=%s",
			doneWaitTarget, doneWaitSeen, doneWaitElapsed, waitTimeout)
		cancelRecovery()
		return
	}
	cancelRecovery()

	// The stale orphan must NOT have been stamped: still unfinished, no
	// FinishReasonError, Parts untouched.
	gotOrphan, err := msgs.Get(ctx, orphan.ID)
	require.NoError(t, err)
	assert.False(t, gotOrphan.IsFinished(),
		"stale candidate must be left unfinished, not retroactively stamped 'Process restarted'")
	assert.Nil(t, gotOrphan.FinishPart(),
		"stale candidate must not gain a finish part it didn't have before recovery ran")

	// The newer message (the session's actual last message) must be
	// untouched by recovery.
	gotNewer, err := msgs.Get(ctx, newer.ID)
	require.NoError(t, err)
	assert.True(t, gotNewer.IsFinished())
	assert.Equal(t, message.FinishReasonEndTurn, gotNewer.FinishReason(),
		"the session's real last message must keep its own finish reason, not be clobbered")
}
