package app

// C9-4: the unfinished-todos reminder turn is optional. When it fails for its
// own reason (a provider error, a lock refusal) it is dropped: the executor's
// last answer stays the run's outcome, one warning names the failure, and no
// further reminders fire.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// REVERT CHECK: removing nudgePhase's drop branch (C9-4) makes the failed
// reminder the run's outcome -- a second reminder request, runErr = the
// provider error, exit_reason "error", and the "ended with unfinished todos"
// warning instead of the nudge-failure warning; this test FAILED on
// require.NoError and requests==2.
func TestRunLoop_TodoNudgeFailureKeepsTheAnswer(t *testing.T) {
	var stderr syncBuffer
	cliLoopStderr = &stderr
	t.Cleanup(func() { cliLoopStderr = nil })

	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		if n == 1 {
			loopText(w, "a", "first answer", 11, 3)
			return
		}
		if n == 2 {
			// The reminder turn: the provider refuses it. 400 is a terminal
			// client error (no coordinator transient-retry, unlike a 403).
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"bad request: model rejected","type":"x"}}`))
			return
		}
		h.t.Errorf("unexpected provider request #%d: the failed reminder must not be retried", n)
		loopText(w, "x", "unexpected", 1, 1)
	})
	require.NoError(t, h.app.Sessions.SetTodos(context.Background(), h.sessionID, []session.Todo{
		{Content: "deploy after the answer", Status: session.TodoStatusPending},
	}, nil))

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	require.NoError(t, err, "a failed optional reminder is not a failed run")
	require.NotNil(t, res)
	require.Equal(t, "first answer", res.FinalText, "the executor's answer stands")
	require.Equal(t, "end_turn", res.ExitReason)
	require.EqualValues(t, 2, h.requests.Load(), "first turn plus ONE reminder, no retry")

	var failed, gaveUp bool
	for _, warn := range res.Warnings {
		if strings.Contains(warn, "todo reminder turn failed") {
			failed = true
		}
		if strings.Contains(warn, "ended with unfinished todos") {
			gaveUp = true
		}
	}
	require.True(t, failed, "one warning names the failed reminder: %v", res.Warnings)
	require.False(t, gaveUp, "the todos-gave-up warning must not fire: %v", res.Warnings)
	require.Contains(t, stderr.String(), "todo reminder turn failed")
	require.Contains(t, stderr.String(), "; the run keeps its last answer")
}

// REVERT CHECK: without the drop branch the refused reminder burns the
// reminder budget -- the run ends with the misleading "ended with unfinished
// todos ... after 2 reminder(s)" warning and "still has ... unfinished
// todo(s)" stderr noise instead of the truthful nudge-failure line; this test
// FAILED on the warning/stderr assertions.
func TestRunLoop_TodoNudgeRefusedByLockKeepsTheAnswer(t *testing.T) {
	origLimit := cliLockBusyRetryOverallLimit
	cliLockBusyRetryOverallLimit = 1500 * time.Millisecond
	t.Cleanup(func() { cliLockBusyRetryOverallLimit = origLimit })
	origPause := cliSetupRetryPause
	cliSetupRetryPause = 25 * time.Millisecond
	t.Cleanup(func() { cliSetupRetryPause = origPause })

	var stderr syncBuffer
	cliLoopStderr = &stderr
	t.Cleanup(func() { cliLoopStderr = nil })

	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		if n >= 2 {
			h.t.Errorf("a refused turn never reaches the provider (request #%d)", n)
		}
		loopText(w, "a", "first answer", 11, 3)
	})
	require.NoError(t, h.app.Sessions.SetTodos(context.Background(), h.sessionID, []session.Todo{
		{Content: "deploy after the answer", Status: session.TodoStatusPending},
	}, nil))
	h.afterFirstTurn(func() {
		foreign, err := session.TryAcquireSessionLock(h.dataDir, h.sessionID)
		require.NoError(t, err)
		t.Cleanup(func() { _ = foreign.Release() })
	})

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	require.NoError(t, err, "the refusal must not fail the run")
	require.NotNil(t, res)
	require.Equal(t, "first answer", res.FinalText)
	require.Equal(t, "end_turn", res.ExitReason)
	require.Empty(t, res.Error)
	require.EqualValues(t, 1, h.requests.Load(), "the refused reminder reaches no provider")
	require.Contains(t, stderr.String(), "todo reminder turn failed")
	require.NotContains(t, stderr.String(), "unfinished todo(s) after")

	var failed, gaveUp bool
	for _, warn := range res.Warnings {
		if strings.Contains(warn, "todo reminder turn failed") {
			failed = true
		}
		if strings.Contains(warn, "ended with unfinished todos") {
			gaveUp = true
		}
	}
	require.False(t, gaveUp, "the todos-gave-up warning must not fire: %v", res.Warnings)
	require.True(t, failed, "one warning names the failed reminder: %v", res.Warnings)
}

// The drop branch sits after the ctx-cancelled branch; this test fails if a
// refactor ever moves it ahead of it.
func TestRunLoop_TodoNudgeCanceledStillExitsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(loopCtx(t))
	defer cancel()

	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		if n == 1 {
			loopText(w, "a", "first answer", 11, 3)
			return
		}
		if n == 2 {
			cancel()
			loopText(w, "n", "nudge text", 30, 6)
			return
		}
		loopText(w, "x", "unexpected", 1, 1)
	})
	require.NoError(t, h.app.Sessions.SetTodos(context.Background(), h.sessionID, []session.Todo{
		{Content: "deploy after the answer", Status: session.TodoStatusPending},
	}, nil))

	res, _, err := h.run(ctx, RunOverrides{})

	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
}

// A reminder turn that itself ends on ask_question is an answer of its own
// kind, not a failed reminder: the run exits awaiting_answer with the question.
//
// REVERT CHECK: dropping the AwaitingAnswerError exclusion from nudgePhase's
// drop branch treats the question as a failed reminder -- the run ends
// end_turn with a "todo reminder turn failed" warning and the question never
// reaches the caller.
func TestRunLoop_TodoNudgeQuestionIsNotAFailedReminder(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		switch n {
		case 1:
			loopText(w, "a", "first answer", 11, 3)
		case 2:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("q", "call_q", "ask_question", `{"question":"which environment?"}`),
				admissionSSEStop("q", "tool_calls"),
			})
		default:
			h.t.Errorf("unexpected provider request #%d: the run must stop on the reminder's question", n)
			loopText(w, "x", "unexpected", 1, 1)
		}
	})
	require.NoError(t, h.app.Sessions.SetTodos(context.Background(), h.sessionID, []session.Todo{
		{Content: "deploy after the answer", Status: session.TodoStatusPending},
	}, nil))

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	var awaiting *agent.AwaitingAnswerError
	require.ErrorAs(t, err, &awaiting, "the reminder's question must reach the caller")
	require.NotNil(t, res)
	require.Equal(t, "awaiting_answer", res.ExitReason)
	require.EqualValues(t, 2, h.requests.Load(), "first turn plus the reminder that asked")
	for _, warn := range res.Warnings {
		require.NotContains(t, warn, "todo reminder turn failed")
	}
}
