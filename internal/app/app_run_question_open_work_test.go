package app

// A question suspends automatic resumption even when completing open work
// creates deferred debt while the run waits for that work.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// REVERT CHECK: removing coord.suspendAutoResume(call.SessionID) in
// internal/agent/drain_attempt.go makes this issue a second provider request.
func TestRunLoop_QuestionWithOpenDelegationExitsAwaitingAnswer(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		if n == 1 {
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("q", "call_q", "ask_question", `{"question":"which environment?"}`),
				admissionSSEStop("q", "tool_calls"),
			})
			return
		}
		h.t.Errorf("unexpected provider request #%d: the run must stop on its question", n)
		loopText(w, "x", "unexpected", 1, 1)
	})
	ctx, cancel := context.WithTimeout(loopCtx(t), 15*time.Second)
	t.Cleanup(cancel)
	child, err := h.app.Sessions.Create(ctx, "child")
	require.NoError(t, err)
	_, err = h.app.asyncJobStore.Claim(ctx, session.ClaimParams{
		Owner: h.sessionID, ToolCallID: "deleg-1", Kind: session.JobKindAgent, Input: "x",
		ChildSessionID: child.ID, ToolName: "agent",
	})
	require.NoError(t, err)
	require.NoError(t, h.app.asyncJobStore.MarkAnnounced(ctx, h.sessionID, "deleg-1"))
	transitionDone := make(chan error, 1)
	h.afterFirstTurn(func() {
		h.seedDebt()
		go func() {
			time.Sleep(300 * time.Millisecond)
			_, transitionErr := h.app.asyncJobStore.Transition(ctx, session.TransitionParams{
				Owner: h.sessionID, ToolCallID: "deleg-1", State: "completed", ResultSummary: "done", Wake: true,
			})
			transitionDone <- transitionErr
		}()
	})

	res, _, runErr := h.run(ctx, RunOverrides{})

	require.NoError(t, ctx.Err(), "the run finishes within ~15s")
	require.NoError(t, <-transitionDone)
	var awaiting *agent.AwaitingAnswerError
	require.ErrorAs(t, runErr, &awaiting)
	require.NotNil(t, res)
	require.Equal(t, "awaiting_answer", res.ExitReason)
	require.EqualValues(t, 1, h.requests.Load())
	require.True(t, h.debtOpen(), "the completion is deferred, never owed: the notice stays for the answer turn")
}
