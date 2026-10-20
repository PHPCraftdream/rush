// A14 e2e (docs/plans/2026-10-01-inline-window.md, design test 7): a `rush
// run` whose model calls the fast `echo hi` through the bash tool gets the
// result in the tool call's own response -- TWO provider calls (turn with
// the tool call, turn with the final text), no pulled notice, no Drain in
// between. With the window seam at 0 (the revert variant below), the same
// script needs THREE calls (the "started" reaction turn in the middle).
package app

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func runInlineWindowE2E(t *testing.T, windowOn bool) (calls int32, backgroundNotices int) {
	t.Helper()
	var requests atomic.Int32
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(string(body), "short title"):
			admissionWriteSSE(w, []string{admissionSSEText("title", "Inline Window"), admissionSSEStop("title", "stop")})
		case strings.Contains(lastTool, "hi"):
			// Turn 2 (window on): the tool result already carries the output.
			requests.Add(1)
			admissionWriteSSE(w, []string{admissionSSEText("done", "all good"), admissionSSEStop("done", "stop")})
		case strings.Contains(lastTool, "Async bash job"):
			// Turn 2 (window 0): the "started" result needs its own
			// composing turn while the job finishes.
			requests.Add(1)
			admissionWriteSSE(w, []string{admissionSSEText("started", "waiting"), admissionSSEStop("started", "stop")})
		case strings.Contains(string(body), "call-1 (bash)"):
			// Turn 3 (window 0): the pulled completion notice's reaction.
			requests.Add(1)
			admissionWriteSSE(w, []string{admissionSSEText("done", "all good"), admissionSSEStop("done", "stop")})
		default:
			// Turn 1: issue the fast command.
			requests.Add(1)
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("start", "call-1", "bash", `{"command":"echo hi","description":"quick job"}`),
				admissionSSEStop("start", "tool_calls"),
			})
		}
	})
	// newAdmissionRaceApp disables the window for its other tests; this
	// scenario is exactly about the window, so set it explicitly.
	require.True(t, agent.SetInlineWindowForTest(application.AgentCoordinator, windowOn))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := application.RunNonInteractiveWithResult(ctx, io.Discard, "run a quick job", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason, "warnings=%v", res.Warnings)
	return requests.Load(), countBackgroundNotices(t, application, sessionID, "")
}

func TestRunNonInlineWindow_FastBashAnsweredInline(t *testing.T) {
	calls, notices := runInlineWindowE2E(t, true)
	require.EqualValues(t, 2, calls, "turn with the tool call + final turn; no started-reaction turn in between")
	require.Equal(t, 0, notices, "the result traveled in the tool result; no completion notice")
}

func testBatchWindowZeroKeepsStartedFlow(t *testing.T) {
	command, release := recoveryJobCommand(t)
	command += "; echo E1-WINDOW-OUTPUT"
	completionText := make(chan string, 1)
	started := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	var launches, startedReplies, completions, finalRequests atomic.Int32
	var yielded sync.Once
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	old := cliLoopTurnDoneSeam
	cliLoopTurnDoneSeam = func() {
		yielded.Do(func() {
			select {
			case <-started:
			case <-ctx.Done():
				return
			}
		})
		releaseOnce.Do(func() { releaseRecoveryJob(t, release) })
	}
	defer func() { cliLoopTurnDoneSeam = old }()
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, lastUser, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(lastUser, "Async job call-1 () finished"):
			completions.Add(1)
			finalRequests.Add(1)
			completionText <- lastUser
			admissionWriteSSE(w, []string{admissionSSEText("final", "window final"), admissionSSEStop("final", "stop")})
		case strings.Contains(lastTool, "completions arrive as selected notices"):
			startedReplies.Add(1)
			startedOnce.Do(func() { close(started) })
			admissionWriteSSE(w, []string{admissionSSEText("yield", "job started"), admissionSSEStop("yield", "stop")})
		case strings.Contains(lastUser, "short title"):
			admissionWriteSSE(w, []string{admissionSSEText("title", "Window test"), admissionSSEStop("title", "stop")})
		default:
			launches.Add(1)
			admissionWriteSSE(w, []string{admissionSSEToolCall("start", "call-1", "bash", `{"command":`+jsonString(command)+`,"description":"held job"}`), admissionSSEStop("start", "tool_calls")})
		}
	})
	t.Cleanup(func() { releaseOnce.Do(func() { releaseRecoveryJob(t, release) }); application.Shutdown() })
	done := make(chan struct{})
	var runErr error
	var result *RunResult
	go func() {
		defer close(done)
		result, runErr = application.RunNonInteractiveWithResult(ctx, io.Discard, "run held job", RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("held job run did not finish")
	}
	require.NoError(t, runErr)
	require.NotNil(t, result)
	require.Equal(t, "window final", result.FinalText)
	require.EqualValues(t, 1, launches.Load())
	require.EqualValues(t, 1, startedReplies.Load())
	require.EqualValues(t, 1, completions.Load())
	require.EqualValues(t, 1, finalRequests.Load())
	select {
	case text := <-completionText:
		require.Contains(t, text, "E1-WINDOW-OUTPUT")
	case <-ctx.Done():
		t.Fatal("completion text not observed")
	}
	require.Equal(t, 1, countBackgroundNotices(t, application, sessionID, ""))
}

func TestRunNonInlineWindow_WindowZeroKeepsStartedFlow(t *testing.T) {
	if f6BatchOn() {
		testBatchWindowZeroKeepsStartedFlow(t)
		return
	}
	calls, notices := runInlineWindowE2E(t, false)
	// Without the window the result can never travel in the tool call's own
	// response: at least one extra provider call past the issuing turn (a
	// fast job whose completion is pulled before the "started" turn's
	// request can fold the reaction into that same turn, hence >=2 rather
	// than exactly 3).
	require.GreaterOrEqual(t, calls, int32(2), "the started flow needs a reaction provider call")
	require.Equal(t, 1, notices, "the completion arrives as a pulled notice")
}
