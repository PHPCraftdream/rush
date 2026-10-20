package app

// A question asked while live work runs no longer suspends the run (#1270):
// the ask_question tool hands back a keep-alive hint and the turn continues.

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRunLoop_QuestionWithLiveDelegationContinuesWithHint replaces the old
// awaiting-answer contract (#1270): ask_question with a live delegation now
// returns the keep-alive hint (the text names await_tasks), the turn CONTINUES
// with that hint as the tool result, and the run does NOT exit awaiting_answer.
//
// REVERT CHECK: restoring suspendAutoResume(call.SessionID) in
// internal/agent/drain_attempt.go (or dropping the live-work branch in
// internal/agent/tools/ask_question.go so the tool returns AskQuestionError
// while the delegation runs) makes the run exit awaiting_answer at request 1 --
// request 2 never happens and this test goes red.
func TestRunLoop_QuestionWithLiveDelegationContinuesWithHint(t *testing.T) {
	var body2 atomic.Value // string
	// The completer goroutine outlives neither the run nor the test: it is
	// joined before any assertion so its require calls can never fire after
	// the test completed.
	var completers sync.WaitGroup
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, _ bool, n int) {
		switch n {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("q", "call_q", "ask_question", `{"question":"which environment?"}`),
				admissionSSEStop("q", "tool_calls"),
			})
			completers.Add(1)
			go func() {
				defer completers.Done()
				time.Sleep(300 * time.Millisecond)
				f6CompleteDelegation(h.t, context.Background(), h.app, h.sessionID, "deleg-1", "done", true)
			}()
		case 2:
			body2.Store(string(body))
			loopText(w, "h", "waiting for the worker", 11, 3)
		default:
			loopText(w, "f", "done", 11, 3)
		}
	})
	ctx, cancel := context.WithTimeout(loopCtx(t), 15*time.Second)
	t.Cleanup(cancel)
	child, err := h.app.Sessions.Create(ctx, "child")
	require.NoError(t, err)
	f6ClaimDelegation(t, ctx, h.app, h.sessionID, "deleg-1", child.ID)

	res, _, runErr := h.run(ctx, RunOverrides{})

	completers.Wait()

	require.NoError(t, runErr)
	require.NoError(t, ctx.Err(), "the run finishes within ~15s")
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason,
		"the hinted question keeps the run alive; it never exits awaiting_answer")
	require.Equal(t, "done", res.FinalText)
	require.EqualValues(t, 3, h.requests.Load(),
		"the question turn, the hint continuation, the completion drain")
	require.Contains(t, body2.Load().(string), "await_tasks",
		"the hint names await_tasks as the way to wait for the delegation")
}
