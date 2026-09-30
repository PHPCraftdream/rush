// W-DRAIN item C (docs/reviews/2026-09-29-async-phase4-round1.md, "Tests
// (B-b, C-b)"): Ctrl-C landing DURING the `rush run` CLI loop's own
// automatic empty-prompt Drain turn (not the first, user-initiated turn --
// see TestTwoAppScenarioC_CtrlCThenRestartInterruptsEveryLiveTask for that
// case) must never settle the reacting debt by failure, must never write a
// wake_failed marker, and the JSON result's exit_reason must read "canceled"
// -- matching doc sec.3.4's "graceful exit = crash" and B5/C3's fix routing
// context.Canceled to the recheck set instead of classifyProviderError.
package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestCLIDrainTurn_CtrlCMidFlight_NoSettleNoMarkerCorrectExitReason drives a
// real single-App `rush run` loop: turn 1 starts a real (~3s) async bash job
// and yields; once the job finishes, the loop's own empty-prompt Drain turn
// fires automatically (doc sec.3.4) and this test cancels the run's ctx
// while THAT specific request is in flight (never during turn 1).
//
// REVERT CHECK: making drainAttemptExempt (agent/drain_attempt.go) return
// false for a plain cancellation counts the interrupted attempt; the debt is
// then paced (wake_attempts=1) instead of untouched, and the
// `wake_attempts == 0` assertion FAILED. Restored the exemption; re-ran, passed.
func TestCLIDrainTurn_CtrlCMidFlight_NoSettleNoMarkerCorrectExitReason(t *testing.T) {
	drainReqReached := make(chan struct{})
	var drainOnce sync.Once
	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(string(body), "call-1 (bash)"):
			// The loop's own automatic Drain turn, reacting to the finished
			// job's notice. Checked FIRST (see scenario E's own doc): once
			// this text is in history it stays there, so lastTool alone
			// would keep matching the "yield" branch below on every later
			// request too. Bounded regardless of the client so this
			// handler always returns and httptest.Server.Close never hangs.
			drainOnce.Do(func() { close(drainReqReached) })
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		case strings.Contains(lastTool, "Async bash job call-1 started"):
			admissionWriteSSE(w, []string{admissionSSEText("yield", "root yielded"), admissionSSEStop("yield", "stop")})
		default:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("start", "call-1", "bash", `{"command":`+jsonString(recoveryBuildCommand())+`,"description":"job 1"}`),
				admissionSSEStop("start", "tool_calls"),
			})
		}
	}

	app, sessionID := newAdmissionRaceApp(t, handler)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	var res *RunResult
	var runErr error
	go func() {
		defer close(runDone)
		res, runErr = app.RunNonInteractiveWithResult(ctx, io.Discard, "start a job", RunOverrides{
			Origin: message.OriginCLI,
		}, true, RunModeJSON, sessionID, false)
	}()

	select {
	case <-drainReqReached:
	case <-time.After(20 * time.Second):
		t.Fatal("the CLI loop's own Drain turn never reached the provider")
	}
	cancel()

	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never returned after ctx cancellation")
	}
	require.Error(t, runErr)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason, "Ctrl-C during a CLI Drain must report exit_reason=canceled, not error/end_turn")

	debtStillOpen, err := app.asyncJobStore.ReactionDebtExists(context.Background(), sessionID)
	require.NoError(t, err)
	require.True(t, debtStillOpen, "Ctrl-C mid-Drain must never settle the debt")
	row, err := app.asyncJobStore.Get(context.Background(), sessionID, "call-1")
	require.NoError(t, err)
	require.EqualValues(t, 0, row.WakeAttempts, "an interrupted Drain is not evidence about the debt: not counted")

	notices, err := app.asyncJobStore.ListSessionNotices(context.Background(), sessionID)
	require.NoError(t, err)
	for _, n := range notices {
		require.NotEqual(t, "wake_failed", n.Kind, "Ctrl-C mid-Drain must never write a settle marker")
	}
}
