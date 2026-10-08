package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// Revert-check: treating ordinary 429 as IsHardQuotaLimit prevents the coordinator retry.
func TestCLIQuotaRetryable429Unchanged(t *testing.T) {
	var firstAt time.Time
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		if n == 1 {
			firstAt = time.Now()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "0.1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"server overloaded","type":"rate_limit","code":"rate_limit"}}`))
			return
		}
		loopText(w, "ok", "retry succeeded", 1, 1)
	})
	res, _, err := h.run(loopCtx(t), RunOverrides{})
	require.NoError(t, err)
	require.Equal(t, "end_turn", res.ExitReason)
	require.GreaterOrEqual(t, time.Since(firstAt), 2*time.Second)
	require.Greater(t, h.requests.Load(), int32(1))
	require.Empty(t, res.ResumeCommand)
}

func quotaResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "3600")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":{"message":"hard usage limit quota exhausted","type":"quota","code":"quota"}}`))
}

// Revert-check: removing cliLoop.latchQuota from firstTurnPhase permits a completion Drain.
func TestCLIQuotaFirstWaitsPersistsAndResumes(t *testing.T) {
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 50*time.Millisecond, 0)()
	var resume bool
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, _ bool, n int) {
		if resume {
			require.Contains(t, string(body), "own-output")
			require.Contains(t, string(body), "child-output")
			loopText(w, "resumed", "resumed persisted results", 1, 1)
			return
		}
		require.LessOrEqual(t, n, 3, "no own turn after quota")
		quotaResponse(w)
	})
	ctx := loopCtx(t)
	child, err := h.app.Sessions.Create(ctx, "child")
	require.NoError(t, err)
	claimDelegation(t, ctx, h.app, h.sessionID, "child-job", child.ID)
	history, err := h.app.Sessions.Create(ctx, "historical child")
	require.NoError(t, err)
	claimDelegation(t, ctx, h.app, h.sessionID, "historical-child", history.ID)
	_, err = h.app.asyncJobStore.Claim(ctx, session.ClaimParams{Owner: h.sessionID, ToolCallID: "own-bash", Kind: session.JobKindCommand, ToolName: "bash", Input: "x"})
	require.NoError(t, err)
	require.NoError(t, h.app.asyncJobStore.MarkAnnounced(ctx, h.sessionID, "own-bash"))
	completeDelegation(t, ctx, h.app, h.sessionID, "historical-child", "historical-output", false)
	h.afterFirstTurn(func() {
		go func() {
			time.Sleep(150 * time.Millisecond)
			_, e := h.app.asyncJobStore.Transition(ctx, session.TransitionParams{Owner: h.sessionID, ToolCallID: "own-bash", State: "completed", ResultSummary: "own-output", Wake: true})
			require.NoError(t, e)
			time.Sleep(150 * time.Millisecond)
			completeDelegation(t, ctx, h.app, h.sessionID, "child-job", "child-output", true)
		}()
	})
	before := time.Now()
	res, wire, runErr := h.run(ctx, RunOverrides{})
	require.Error(t, runErr)
	require.Equal(t, "provider_limit", res.ExitReason)
	require.EqualValues(t, 3, h.requests.Load())
	require.Len(t, res.FinishedWork, 2)
	for _, work := range res.FinishedWork {
		require.NotEqual(t, "historical-child", work.JobID)
	}
	require.Contains(t, wire, "own-output")
	require.Contains(t, wire, "child-output")
	require.Contains(t, res.ResumeCommand, "--role smart --session "+h.sessionID)
	reset, err := time.Parse(time.RFC3339, res.QuotaResetAt)
	require.NoError(t, err)
	require.WithinDuration(t, before.Add(time.Hour), reset, 10*time.Second)
	jobs, err := h.app.asyncJobStore.ListAsyncJobsForOwner(ctx, h.sessionID)
	require.NoError(t, err)
	for _, job := range jobs {
		if job.ToolCallID != "historical-child" {
			require.Equal(t, "pending", job.Delivery)
		}
	}
	resume = true
	res, _, runErr = h.run(ctx, RunOverrides{})
	require.NoError(t, runErr)
	require.Equal(t, "resumed persisted results", res.FinalText)
	jobs, err = h.app.asyncJobStore.ListAsyncJobsForOwner(ctx, h.sessionID)
	require.NoError(t, err)
	for _, job := range jobs {
		require.Equal(t, "done", job.Delivery)
	}
}

// Revert-check: removing cliLoop.quotaPhase from decidePhase waits for the future once schedule.
func TestCLIQuotaDrainIgnoresFutureAwaitSchedule(t *testing.T) {
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 50*time.Millisecond, 0)()
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, n int) {
		if n == 1 {
			h.seedDebt()
			loopText(w, "first", "keep answer", 1, 1)
			return
		}
		require.LessOrEqual(t, n, 4)
		quotaResponse(w)
	})
	ctx := loopCtx(t)
	scheduler := h.app.AgentCoordinator.(tools.WakeControl)
	_, err := scheduler.ScheduleWakeOnceDelay(ctx, h.sessionID, 3600, "await_tasks max_wait elapsed")
	require.NoError(t, err)
	start := time.Now()
	res, wire, runErr := h.run(ctx, RunOverrides{})
	require.Error(t, runErr)
	require.Equal(t, "provider_limit", res.ExitReason)
	require.Equal(t, "keep answer", res.FinalText)
	require.EqualValues(t, 4, h.requests.Load())
	require.Less(t, time.Since(start), 15*time.Second)
	require.Contains(t, wire, "once schedules")
	require.True(t, cliScopeOf(t, h.app, h.sessionID).OnceWakeOpen)
}

// Revert-check: removing quotaPhase's durable finish metadata read drops pending child questions.
func TestCLIQuotaPreservesPendingChildQuestion(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) { quotaResponse(w) })
	ctx := loopCtx(t)
	child, err := h.app.Sessions.Create(ctx, "question child")
	require.NoError(t, err)
	claimDelegation(t, ctx, h.app, h.sessionID, "held-child", child.ID)
	_, err = h.app.Messages.Create(ctx, child.ID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{
		message.Finish{Reason: message.FinishReasonError, Message: "Stopped: agent asked a question and is awaiting an answer", Details: agent.AwaitingAnswerGuidance("which port?", []string{"8080"}, child.ID)},
	}})
	require.NoError(t, err)
	res, wire, runErr := h.run(ctx, RunOverrides{})
	require.Error(t, runErr)
	require.Equal(t, "provider_limit", res.ExitReason)
	require.Len(t, res.PendingChildQuestions, 1)
	require.Contains(t, wire, "which port?")
	require.EqualValues(t, 3, h.requests.Load())
	var decoded RunResult
	require.NoError(t, json.Unmarshal([]byte(wire), &decoded))
	require.Contains(t, strings.Join([]string{decoded.Error, decoded.ResumeCommand}, " "), "--role smart")
}
