package app

// C9-2 (ox round 9): a turn that ends on ask_question makes `rush run` exit
// with awaiting_answer -- the process will not retry or re-ask. The unfinished-
// todos reminder (A18) fired at that same scope close and sent a prompt the
// caller never wrote on top of the unanswered question; the run then ended
// end_turn with exit code 0 and the question never reached the caller.

import (
	"context"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// REVERT CHECK: dropping the AwaitingAnswerError guard from todoNudgeDue fires a
// reminder turn after the question -- this test FAILED (a second provider
// request, exit_reason end_turn, no AwaitingAnswerError).
func TestRunNonInteractive_OpenTodosDoNotBuryAnAskedQuestion(t *testing.T) {
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
	require.NoError(t, h.app.Sessions.SetTodos(context.Background(), h.sessionID, []session.Todo{
		{Content: "deploy after the answer", Status: session.TodoStatusPending},
	}, nil))

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	var awaiting *agent.AwaitingAnswerError
	require.ErrorAs(t, err, &awaiting, "the question must reach the caller")
	require.NotNil(t, res)
	require.Equal(t, "awaiting_answer", res.ExitReason)
	require.EqualValues(t, 1, h.requests.Load(), "no reminder turn on top of an unanswered question")
}
