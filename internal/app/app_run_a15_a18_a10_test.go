// Anomalies A15 (#1135), A18 (#1145), A10 (#1146) — how and with what a
// `rush run` invocation ENDS. All three are end-to-end on the real loop
// harness (app_run_loop_test.go's App + coordinator + SQLite + httptest
// provider).
package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// A15: a question asked while the session's OWN background work is running
// must not end the run. The tool hands the model a hint, the model finishes
// the turn without tool calls, and the run exits normally — not with
// awaiting_answer and an orphaned job.
//
// Revert-check: removing the own-running-jobs carve-out in ask_question.go
// (or the reader wiring in prepareExecuteRun) makes the first turn
// force-finish with awaiting_answer and this test goes red.
func TestRunNonInteractive_AskQuestionWithRunningOwnJob_ContinuesRun(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, drain bool, n int) {
		switch n {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("q", "call_q", "ask_question", `{"question":"continue while the heavy run finishes?"}`),
				admissionSSEStop("q", "tool_calls"),
			})
		case 2:
			// The job is still running when the tool answers (that is the
			// point); by the time the turn is over it must be done, or the
			// loop would rightly wait on open work forever.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := h.app.asyncJobStore.Transition(ctx, session.TransitionParams{
				Owner: h.sessionID, ToolCallID: "bg-1", State: "completed", ResultSummary: "heavy run done", Wake: false,
			})
			require.NoError(h.t, err)
			loopText(w, "c", "continued after hint", 5, 5)
		default:
			t.Errorf("unexpected provider request #%d", n)
			loopText(w, "x", "unexpected", 1, 1)
		}
	})

	ctx := loopCtx(t)
	_, err := h.app.asyncJobStore.Claim(ctx, session.ClaimParams{
		Owner: h.sessionID, ToolCallID: "bg-1", Kind: session.JobKindCommand, Input: "heavy", ToolName: "bash",
	})
	require.NoError(t, err)

	res, _, runErr := h.run(ctx, RunOverrides{})

	require.NoError(t, runErr)
	require.NotNil(t, res)
	require.NotEqual(t, "awaiting_answer", res.ExitReason,
		"a question over running own work must not end the run")
	require.Equal(t, "continued after hint", res.FinalText,
		"the model must get the hint and finish the turn itself")
	require.EqualValues(t, 2, h.requests.Load(), "the ask and the post-hint continuation")
}

// A18, happy path: the first turn ends on an announcement while todos are
// open; the loop fires ONE reminder turn, the model closes the todos, and
// the run ends clean with the reminder turn's answer as final_text.
//
// Revert-check: deleting the todoNudgeDue gate in decidePhase's stepExit
// branch (or the phaseNudge transition) keeps the run at one turn and this
// test goes red on request count.
func TestRunNonInteractive_OpenTodos_FiresReminderThenEnds(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, drain bool, n int) {
		if n == 1 {
			loopText(w, "a", "Now session_update.go:", 11, 3)
			return
		}
		require.Contains(h.t, string(body), "unfinished",
			"the reminder turn must carry the unfinished-todos prompt")
		// The model acts on the reminder: the todos close, so the next
		// close check finds nothing open.
		require.NoError(h.t, h.app.Sessions.SetTodos(context.Background(), h.sessionID, []session.Todo{
			{Content: "write session_update.go", Status: session.TodoStatusCompleted},
		}, nil))
		loopText(w, "r", "kept working", 7, 3)
	})
	require.NoError(t, h.app.Sessions.SetTodos(context.Background(), h.sessionID, []session.Todo{
		{Content: "write session_update.go", Status: session.TodoStatusPending},
	}, nil))

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.EqualValues(t, 2, h.requests.Load(), "the first turn and exactly one reminder")
	require.Equal(t, "kept working", res.FinalText)
	require.Empty(t, resWarningsContaining(t, res, "unfinished todos"),
		"todos closed: no give-up warning")
}

// A18, the fuse: the model answers every reminder with another announcement
// and the todos never move. After cliTodoNudgeLimit reminders the run ends
// anyway, with a warning in the envelope.
//
// Revert-check: raising the limit (or resetting todoNudges without a
// fingerprint change) makes the run wait for a third reminder and this test
// goes red on request count / missing warning.
func TestRunNonInteractive_UnmovedTodos_LimitEndsWithWarning(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, drain bool, n int) {
		if n == 1 {
			loopText(w, "a", "partial", 11, 3)
			return
		}
		require.Contains(h.t, string(body), "unfinished")
		loopText(w, "r", "still busy", 7, 3)
	})
	require.NoError(t, h.app.Sessions.SetTodos(context.Background(), h.sessionID, []session.Todo{
		{Content: "never done", Status: session.TodoStatusPending},
	}, nil))

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.EqualValues(t, 1+cliTodoNudgeLimit, h.requests.Load(),
		"the first turn plus exactly the reminder budget")
	warns := resWarningsContaining(t, res, "unfinished todos")
	require.Len(t, warns, 1, "the give-up must be visible in the envelope warnings")
	require.Contains(t, warns[0], "ended with unfinished todos")
}

// A18, no-op: closed todos never fire a reminder.
func TestRunNonInteractive_ClosedTodos_NoReminder(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, drain bool, n int) {
		require.EqualValues(t, 1, n, "only the first turn may run")
		loopText(w, "a", "first answer", 11, 3)
	})
	require.NoError(t, h.app.Sessions.SetTodos(context.Background(), h.sessionID, []session.Todo{
		{Content: "done already", Status: session.TodoStatusCompleted},
	}, nil))

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.EqualValues(t, 1, h.requests.Load())
	require.Equal(t, "first answer", res.FinalText)
}

// A10, loop path: with a reviewer configured, a clean --role smart run's
// reviewer pass runs at the loop's scope-closed exit, but the invocation's
// final_text stays the EXECUTOR's answer; the verdict lands in `review`.
//
// Revert-check: restoring closePhase's `l.final = result` on a clean review
// turn makes final_text the verdict and this test goes red.
func TestRunLoop_ReviewerPassKeepsExecutorFinalText(t *testing.T) {
	h := newReviewerPassApp(t, true)
	sess := createModelOverrideSession(t, h.app, "loop-review-a10")

	var out syncBuffer
	res, err := h.app.RunNonInteractiveWithResult(context.Background(), &out,
		"do it", RunOverrides{ModelRole: config.SelectedModelTypeSmart, Origin: message.OriginCLI},
		true, RunModeJSON, sess.ID, false)

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, []string{"smart-default", reviewerPassReviewerModel}, h.requestedModels(),
		"the executor turn plus exactly one reviewer pass")
	require.Equal(t, reviewerPassPrimaryText, res.FinalText,
		"final_text must stay the executor's report (A10)")
	require.Equal(t, reviewerPassReviewerText, res.Review,
		"the verdict must land in the review field (A10)")
	require.Contains(t, out.String(), reviewerPassPrimaryText,
		"the flushed JSON envelope carries the executor's final_text")
	require.Contains(t, out.String(), `"review":"`+reviewerPassReviewerText+`"`,
		"the verdict is on the wire in the review field")
}

func resWarningsContaining(t *testing.T, res *RunResult, substr string) []string {
	t.Helper()
	var out []string
	for _, w := range res.Warnings {
		if strings.Contains(w, substr) {
			out = append(out, w)
		}
	}
	return out
}
