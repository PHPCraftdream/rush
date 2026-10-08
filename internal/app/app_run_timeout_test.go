package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// Revert-check: cliLoop.finish's classifyRunTimeout pins owned timeout at live-job, drain, reviewer and quota waits.
func TestRunTimeoutWaitPhases(t *testing.T) {
	for _, phase := range []string{"live-job", "drain", "reviewer", "quota"} {
		t.Run(phase, func(t *testing.T) {
			block := make(chan struct{})
			h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, body []byte, drain bool, n int) {
				if (phase == "drain" && drain) || (phase == "reviewer" && strings.Contains(string(body), "independent reviewer")) {
					<-block
					return
				}
				if phase == "quota" {
					quotaResponse(w)
					return
				}
				loopText(w, "first", "stored answer", 1, 1)
			})
			t.Cleanup(func() { close(block) })
			if phase == "reviewer" {
				h.app.config.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
			}
			if phase == "drain" {
				h.afterFirstTurn(h.seedDebt)
			}
			if phase == "live-job" || phase == "quota" {
				_, err := h.app.asyncJobStore.Claim(t.Context(), session.ClaimParams{Owner: h.sessionID, ToolCallID: "held", Kind: session.JobKindCommand, ToolName: "bash", Input: "held"})
				require.NoError(t, err)
			}
			duration := 3 * time.Second
			if phase == "quota" {
				duration = 10 * time.Second // allow the provider's 2s/4s retries to latch quota before the wait
			}
			cause := &agent.RunTimeoutCause{Duration: duration, Source: "--timeout"}
			ctx, cancel := context.WithTimeoutCause(t.Context(), cause.Duration, cause)
			defer cancel()
			res, out, err := h.run(ctx, RunOverrides{ModelRole: config.SelectedModelTypeSmart})
			var timeoutErr *RunTimeoutError
			require.ErrorAs(t, err, &timeoutErr)
			require.Same(t, cause, timeoutErr.Cause)
			require.Equal(t, "timeout", res.ExitReason)
			require.Equal(t, "rush run --role smart --session "+h.sessionID, res.ResumeCommand)
			require.Equal(t, 1, strings.Count(out, `"exit_reason"`))
			var wire RunResult
			require.NoError(t, json.Unmarshal([]byte(out), &wire))
			require.Equal(t, res.ResumeCommand, wire.ResumeCommand)
			require.Equal(t, "timeout", wire.ExitReason)
			sess, getErr := h.app.Sessions.Get(context.Background(), h.sessionID)
			require.NoError(t, getErr)
			require.Equal(t, "timeout", sess.EndedReason)
			if phase == "drain" {
				require.True(t, h.debtOpen())
				require.Zero(t, h.markers())
				require.Greater(t, h.drains.Load(), int32(0))
			}
			if phase == "reviewer" {
				require.Greater(t, h.requests.Load(), int32(1))
			}
			if phase == "live-job" {
				require.EqualValues(t, 1, h.requests.Load())
			}
			if phase == "quota" {
				require.EqualValues(t, 3, h.requests.Load(), "quota latched before waiting on the held job")
			}
		})
	}
}

// Revert-check: cliLoop.exitWait's live-context DeadlineExceeded guard pins error, not canceled/timeout.
func TestRunTimeoutInnerDeadlineWithLiveContext(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "first", "answer", 1, 1)
	})
	var out bytes.Buffer
	l := &cliLoop{app: h.app, ctx: t.Context(), output: &out, mode: RunModeJSON, sessionID: h.sessionID, final: &RunResult{SessionID: h.sessionID}}
	res, err := l.exitWait(context.DeadlineExceeded)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	var timeoutErr *RunTimeoutError
	require.False(t, errors.As(err, &timeoutErr))
	require.NoError(t, l.ctx.Err())
	require.Equal(t, "error", res.ExitReason)
	require.Empty(t, res.ResumeCommand)
	require.Equal(t, "error", sessionEndedReason(t, h.app, h.sessionID))
}

// Revert-check: classifyRunTimeout's owned-cause guard leaves a parent deadline canceled without a resume command.
func TestRunTimeoutParentDeadlineRemainsCanceled(t *testing.T) {
	parent, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	ctx, stop := context.WithTimeoutCause(parent, time.Hour, &agent.RunTimeoutCause{
		Duration: time.Hour, Source: "default cap (RUSH_RUN_DEFAULT_HARD_TIMEOUT; no --timeout set)", DefaultCap: true,
	})
	defer stop()
	final := &RunResult{SessionID: "parent-deadline", ExitReason: "canceled"}
	res, err := classifyRunTimeout(ctx, final, ctx.Err(), final.SessionID, "smart")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, context.Cause(ctx), context.DeadlineExceeded)
	var timeoutErr *RunTimeoutError
	require.False(t, errors.As(err, &timeoutErr))
	require.Same(t, final, res)
	require.Equal(t, "canceled", res.ExitReason)
	require.Empty(t, res.Error)
	require.Empty(t, res.ResumeCommand)
}

// Revert-check: classifyRunTimeout's owned-cause guard preserves caller cancellation in cliLoop.finish.
func TestRunTimeoutCallerCancelRemainsCanceled(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "first", "answer", 1, 1)
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.afterFirstTurn(cancel)
	res, _, err := h.run(ctx, RunOverrides{})
	require.ErrorIs(t, err, context.Canceled)
	var timeoutErr *RunTimeoutError
	require.False(t, errors.As(err, &timeoutErr))
	require.Equal(t, "canceled", res.ExitReason)
	require.Empty(t, res.ResumeCommand)
}
