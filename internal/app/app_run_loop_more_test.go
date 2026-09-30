// Loop decision table, busy-session envelope, deferred-debt reviewer and the
// delegated-child retry (B5, B7, B11); see app_run_loop_test.go.
package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// cancelFlagSessions answers IsCancelRequested from a fixed value; every
// other session.Service method is unset (nextStep reads only the flag).
type cancelFlagSessions struct {
	session.Service
	canceled bool
}

func (c cancelFlagSessions) IsCancelRequested(context.Context, string) (bool, error) {
	return c.canceled, nil
}

func nextStepLoop(src agent.ReactionDebtSource) *cliLoop {
	return nextStepLoopCancel(src, false)
}

func nextStepLoopCancel(src agent.ReactionDebtSource, canceled bool) *cliLoop {
	return &cliLoop{
		ctx: context.Background(), source: src, sessionID: "sess-1",
		app: &App{Sessions: cancelFlagSessions{canceled: canceled}},
	}
}

// TestNextStep_DecisionTable pins how the loop reads each CLIScope answer:
// Owed runs a Drain at once, Paced waits (never launches), running work is
// waited on, Stuck and Deferred end the run only when nothing is running.
//
// Revert-check: treating Paced like Owed launches without waiting (the paced
// row goes red); treating Deferred like open work never ends the run.
func TestNextStep_DecisionTable(t *testing.T) {
	st := func(work bool, d agent.DrainState) scopeAnswer {
		return scopeAnswer{state: agent.CLIScopeState{WorkOpen: work, Drain: d, Reason: "why"}}
	}
	cases := []struct {
		name      string
		script    []scopeAnswer
		want      cliStep
		wantWaits int32
	}{
		{"owed runs a Drain at once", []scopeAnswer{st(false, agent.DrainOwed)}, stepDrain, 0},
		{"owed beside running work still runs it", []scopeAnswer{st(true, agent.DrainOwed)}, stepDrain, 0},
		{"paced waits, then the gate opens", []scopeAnswer{st(false, agent.DrainPaced), st(false, agent.DrainOwed)}, stepDrain, 1},
		{"running work is waited on, then the scope closes", []scopeAnswer{st(true, agent.DrainNone), st(true, agent.DrainNone), st(false, agent.DrainNone)}, stepExit, 2},
		{"nothing outstanding ends the run", []scopeAnswer{st(false, agent.DrainNone)}, stepExit, 0},
		{"deferred debt with nothing running ends the run", []scopeAnswer{st(false, agent.DrainDeferred)}, stepExit, 0},
		{"deferred debt beside running work waits for the work", []scopeAnswer{st(true, agent.DrainDeferred), st(false, agent.DrainDeferred)}, stepExit, 1},
		{"stuck with nothing running gives up", []scopeAnswer{st(false, agent.DrainStuck)}, stepStuck, 0},
		{"stuck beside running work waits for the work", []scopeAnswer{st(true, agent.DrainStuck), st(false, agent.DrainStuck)}, stepStuck, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &scriptedScopeSource{script: tc.script}
			step, _, err := nextStepLoop(src).nextStep()
			require.NoError(t, err)
			require.Equal(t, tc.want, step)
			require.Equal(t, tc.wantWaits, src.waits.Load())
		})
	}
}

// R7C-1: a pending `sessions cancel` ends the wait on running work or on the
// launch gate before it starts (stepCanceled, no WaitForHint); an Owed Drain is
// still returned as such (the precheck before the paid turn reads the flag).
//
// Revert-check: without the cancel check in the WorkOpen / Paced branch the
// matching row waits (waits == 1) and returns stepDrain/stepExit instead.
func TestNextStep_CancelRequestedEndsTheWait(t *testing.T) {
	cases := []struct {
		name  string
		state agent.CLIScopeState
		want  cliStep
	}{
		{"running work", agent.CLIScopeState{WorkOpen: true}, stepCanceled},
		{"paced gate", agent.CLIScopeState{Drain: agent.DrainPaced, RetryAt: time.Now().Add(time.Hour)}, stepCanceled},
		{"owed drain is left to the precheck", agent.CLIScopeState{Drain: agent.DrainOwed}, stepDrain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &scriptedScopeSource{script: []scopeAnswer{{state: tc.state}, {state: agent.CLIScopeState{}}}}
			step, _, err := nextStepLoopCancel(src, true).nextStep()
			require.Equal(t, tc.want, step)
			require.Zero(t, src.waits.Load(), "no wait starts once the cancel is pending")
			if tc.want == stepCanceled {
				var inc *runIncompleteError
				require.ErrorAs(t, err, &inc)
				require.Equal(t, "canceled", inc.reason)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestNextStep_PersistentDBError_BoundedWithVisibleError is C17's fix: a
// persistently unreadable DB must not retry forever -- it gives up after a
// bound with an error the caller surfaces.
//
// Revert-check: removing the bound check leaves the retry loop running until
// the test times out.
func TestNextStep_PersistentDBError_BoundedWithVisibleError(t *testing.T) {
	origLimit, origPause := cliDBErrorRetryOverallLimit, cliDBRetryPause
	cliDBErrorRetryOverallLimit = 50 * time.Millisecond
	cliDBRetryPause = 5 * time.Millisecond
	t.Cleanup(func() { cliDBErrorRetryOverallLimit, cliDBRetryPause = origLimit, origPause })

	src := &scriptedScopeSource{script: []scopeAnswer{{err: errors.New("database is locked")}}}
	_, _, err := nextStepLoop(src).nextStep()
	require.Error(t, err, "a persistently unreadable DB must surface as an error, not hang")
	require.Greater(t, src.calls.Load(), int32(1), "must have actually retried")
}

// B7: `rush run --session <busy> --json` on a session another process holds
// fails fast, still prints the JSON envelope and still runs --on-finish.
//
// Revert-check: returning the (nil) final without flushing skips the envelope
// and the hook and this test goes red.
func TestRunNonInteractive_BusyFirstTurnJSONEnvelopeAndHook(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, _ http.ResponseWriter, _ []byte, _ bool, _ int) {
		t.Error("provider must never be called: the lock refusal is caught before any turn runs")
	})
	foreignLock, err := session.TryAcquireSessionLock(h.dataDir, h.sessionID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = foreignLock.Release() })
	marker := filepath.Join(t.TempDir(), "hook.txt")
	hook := "echo $RUSH_SESSION_ID > " + marker
	if runtime.GOOS == "windows" {
		hook = "echo %RUSH_SESSION_ID%> " + marker
	}

	res, out, runErr := h.run(loopCtx(t), RunOverrides{OnFinishHook: hook})

	var lockBusy *session.SessionLockBusyError
	require.ErrorAs(t, runErr, &lockBusy)
	require.NotNil(t, res, "the busy refusal still yields an envelope")
	require.Contains(t, out, `"session_id"`, "the JSON envelope is printed")
	require.Contains(t, out, h.sessionID)
	data, readErr := os.ReadFile(marker)
	require.NoError(t, readErr, "--on-finish still runs")
	require.Contains(t, string(data), h.sessionID)
}

// B11: debt the policy defers (a bg-shell completion with auto-resume off:
// nothing will ever act on it) does not silence the automatic reviewer pass.
//
// Revert-check: counting deferred debt as blocking (the old policy-blind
// ScopeOpen gate) skips the reviewer and this test goes red.
func TestRunNonInteractive_ReviewerRunsWithDeferredDebt(t *testing.T) {
	var reviewerRequests atomic.Int32
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, _ bool, n int) {
		_, lastUser, _ := lastTurnParts(body)
		if strings.Contains(lastUser, "independent reviewer") {
			reviewerRequests.Add(1)
			loopText(w, "r", "REVIEWER VERDICT", 11, 3)
			return
		}
		if n == 1 {
			// The shell finished while the turn ran, after its last pull.
			require.NoError(h.t, h.app.asyncJobStore.InsertSessionNotice(context.Background(), h.sessionID,
				session.NoticeKindBGShellDone, "background shell finished: exit 0", true, ""))
		}
		loopText(w, "a", "PRIMARY ANSWER", 11, 3)
	})
	h.app.config.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: "probe"})

	res, _, err := h.run(loopCtx(t), RunOverrides{ModelRole: config.SelectedModelTypeSmart})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.EqualValues(t, 1, reviewerRequests.Load(), "the reviewer pass runs beside debt nothing will act on")
	require.Equal(t, "REVIEWER VERDICT", res.FinalText, "the review turn's result is the run's answer")
}

// B5: a delegated child's reaction turn fails once (a transient 403); the
// `rush run` process retries it with the same 60s pass the web process runs
// (shrunk here), so the run completes with the child's own reaction instead of
// hanging until --timeout.
//
// Revert-check: dropping StartRecheckTicker from ClaimExternalDriver leaves the
// failed child Drain in a recheck set nothing drains and the run hits its
// deadline.
func TestRunNonInteractive_ChildFailedDrainRetriedByTick(t *testing.T) {
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 0, 200*time.Millisecond)()
	childJobKilled := make(chan struct{})
	var failedOnce, killedOnce atomic.Bool
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch routeModelRequest(body) {
		case "title":
			admissionWriteSSE(w, []string{admissionSSEText("title", "Retry Child"), admissionSSEStop("title", "stop")})
		case "root:final":
			select {
			case <-childJobKilled:
			case <-time.After(30 * time.Second):
				http.Error(w, "root resumed before the child's reaction", http.StatusServiceUnavailable)
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("root-final", "root final answer"), admissionSSEStop("root-final", "stop")})
		case "child:final":
			if failedOnce.CompareAndSwap(false, true) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"message":"forbidden by the fronting balancer","type":"x"}}`))
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("child-result", "child final: gate job reported"), admissionSSEStop("child-result", "stop")})
		case "child:yield":
			if killedOnce.CompareAndSwap(false, true) {
				close(childJobKilled)
			}
			admissionWriteSSE(w, []string{admissionSSEText("child-yield", "child yielded: gate still running"), admissionSSEStop("child-yield", "stop")})
		case "child:kill":
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("child-kill", "call-kill", "job_kill", `{"shell_id":"001"}`),
				admissionSSEStop("child-kill", "tool_calls"),
			})
		case "child:bash":
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("child-bash", "call-bash", "bash",
					`{"command":`+jsonString(longRunningCommand())+`,"description":"hold the gate open","run_in_background":true}`),
				admissionSSEStop("child-bash", "tool_calls"),
			})
		case "root:yield":
			admissionWriteSSE(w, []string{admissionSSEText("root-yield", "root yielded: delegation parked"), admissionSSEStop("root-yield", "stop")})
		default:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("delegate", "call-agent", "agent", `{"prompt":"WORKER-MARKER do the task"}`),
				admissionSSEStop("delegate", "tool_calls"),
			})
		}
	})
	grantChildBackgroundShellTools(t, application)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	var out syncBuffer
	res, err := application.RunNonInteractiveWithResult(ctx, &out, "start a worker", RunOverrides{Origin: message.OriginCLI},
		true, RunModeJSON, sessionID, false)

	require.NoError(t, err, "the run completes; it must not hang until the deadline")
	require.NoError(t, ctx.Err())
	require.NotNil(t, res)
	require.True(t, failedOnce.Load(), "the child's first reaction turn failed")
	require.Equal(t, "root final answer", res.FinalText)
	msgs, listErr := application.Messages.List(t.Context(), sessionID)
	require.NoError(t, listErr)
	found := false
	for _, m := range msgs {
		if m.BackgroundJobNotice && strings.Contains(m.FullText(), "child final") {
			found = true
		}
	}
	require.True(t, found, "the parent's delegation notice carries the child's retried reaction")
}
