package app

// Cancel propagation: the root's own external safety valve (--timeout,
// Ctrl-C — internal/cmd/run.go cancels exactly the ctx passed to
// RunNonInteractiveWithResult) must end the run, but a delegated child
// sub-agent's turn is deliberately NOT tied to that ctx — asyncTool.Run
// detaches the delegated job's context via context.WithoutCancel
// (internal/agent/async_tool.go:55) so an in-flight delegation survives a
// turn-level cancellation. What actually closes the scope down to nothing
// outstanding is what a real `rush run` process does NEXT: cmd/run.go's
// `defer a.Shutdown()` unconditionally calls CancelAgents ->
// AgentCoordinator.CancelAll (internal/app/app_lifecycle.go), which sweeps
// EVERY mailbox — root and every descendant alike (sessionAgent.CancelAll,
// internal/agent/agent_control.go:161) — and hard-stops each one's
// generation and dispatcher contexts.
//
// This test drives both steps explicitly, mirroring the real CLI sequence,
// and observes the child's liveness directly through its own blocked
// provider request (childHandlerUnblocked) rather than through
// AgentCoordinator.IsSessionBusy: a delegated sub-agent's turn runs on a
// SEPARATE *sessionAgent built fresh by coordinator.buildAgent inside
// agentTool() (internal/agent/agent_tool.go, internal/agent/
// coordinator_tools.go:27), not on the root's own c.currentAgent — so
// IsSessionBusy(childSessionID), which only ever queries the ROOT's
// sessionAgent, cannot see the child's mailbox at all and would always
// report false regardless of the child's real state. The child's own HTTP
// request lifecycle is the only externally observable proxy for "is the
// child's turn still alive" available at this level.
//
// TIMING NOTE (found empirically, not guessed): the root's OWN "delegate"
// tool call and its immediate post-tool-result continuation ("root
// yielded...") are TWO STEPS OF THE SAME fantasy tool-use loop inside ONE
// app.ExecuteRun call — the app-level async-completion drain loop
// (app_run_async.go) only starts a NEW ExecuteRun call once THAT first call
// has fully returned. Cancelling ctx while that first call is still
// in-flight (e.g. immediately once the child is confirmed mid-turn, before
// the root's own continuation step has been served) makes ExecuteRun return
// a nil *RunResult with a bare context.Canceled error, and
// runNonInteractiveWithAsyncResults' early-return branch
// (`if sessionID == "" || ctx.Err() != nil { return final, runErr }`) then
// returns that nil result — a require.NotNil(t, result) failure, not a
// product bug. So this test waits for the ROOT's OWN mailbox to go idle
// (IsSessionBusy(rootSessionID) — the CORRECT, same-*sessionAgent instance
// check) before cancelling: the child is guaranteed to still be blocked at
// that point (its handler never returns except on cancellation), so this
// does not race the property under test, and it is not a "root finishes
// while descendant work is live" claim — the OUTER run (RunNonInteractiveWithResult)
// stays open per ASYNC-02 the whole time; only the root's own individual
// mailbox-owned generation, a different layer, has gone idle.
//
// LAW PINNED: docs/async-invariants.md ASYNC-08, first half ("cancellation
// of scope cancels its tasks and child scopes"), for the case where scope
// cancellation is driven by the CLI's own ctx-cancel + Shutdown sequence
// rather than an explicit mid-run Cancel(sessionID) call.
//
// REVERT CHECK: in internal/agent/agent_control.go's (*sessionAgent).CancelAll,
// change the `for _, mb := range a.mailboxes.Seq2()` sweep to skip any
// mailbox other than the one CancelAll's own caller cares about (e.g. only
// latch/cancel a single sessionID instead of every mailbox). The child's
// blocked provider request then never observes context cancellation, the
// final require.Eventually below times out, and childHandlerUnblocked stays
// false.
//
// Run with: go test ./internal/app -run TestRunNonInteractiveCtxCancelThenCancelAllStopsLiveChildTurn -v

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

const (
	cancelDelegCallID = "call-deleg-cancel"
	cancelChildMarker = "CANCEL-CHILD-MARKER"
)

func routeCancelPropagationRequest(body []byte) string {
	system, lastUser, lastTool := lastTurnParts(body)
	switch {
	case strings.Contains(system, "short title"):
		return "title"
	case strings.Contains(lastTool, "Async agent job "+cancelDelegCallID+" started"):
		return "root:yield"
	case strings.Contains(lastUser, cancelChildMarker):
		return "child:hang"
	default:
		return "root:delegate"
	}
}

func TestRunNonInteractiveCtxCancelThenCancelAllStopsLiveChildTurn(t *testing.T) {
	childStarted := make(chan struct{})
	var once sync.Once
	var childHandlerUnblocked atomic.Bool

	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch routeCancelPropagationRequest(body) {
		case "title":
			admissionWriteSSE(w, []string{admissionSSEText("title", "Cancel Propagation"), admissionSSEStop("title", "stop")})
		case "root:yield":
			admissionWriteSSE(w, []string{admissionSSEText("root-yield", "root yielded: waiting"), admissionSSEStop("root-yield", "stop")})
		case "child:hang":
			// The child's own, real in-flight generation: hold it open until
			// its request context is cancelled, exactly like the proven
			// pattern in p421_p0_1_interrupt_live_continuation_test.go.
			once.Do(func() { close(childStarted) })
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			fmt.Fprintf(w, "data: %s\n\n", `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"child working"},"finish_reason":null}]}`)
			if fl != nil {
				fl.Flush()
			}
			<-r.Context().Done()
			childHandlerUnblocked.Store(true)
		default:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("root-deleg", cancelDelegCallID, "agent",
					`{"prompt":"`+cancelChildMarker+` do work"}`),
				admissionSSEStop("root-deleg", "tool_calls"),
			})
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	var result *RunResult
	var runErr error
	go func() {
		defer close(runDone)
		result, runErr = application.RunNonInteractiveWithResult(ctx, io.Discard, "start cancel test", RunOverrides{
			Origin: message.OriginCLI,
		}, true, RunModeJSON, sessionID, false)
	}()

	select {
	case <-childStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("the delegated child never reached the provider; cannot stage the mid-work cancellation")
	}

	// Wait for the root's OWN multi-step tool-use loop (delegate -> tool
	// result -> continuation, all one ExecuteRun call) to finish and its
	// mailbox to go idle — see the TIMING NOTE above for why cancelling
	// before this point makes ExecuteRun return a nil result instead of
	// exercising the property under test. The child cannot have finished on
	// its own in the meantime: its handler only returns on ctx cancellation.
	require.Eventually(t, func() bool {
		return !application.AgentCoordinator.IsSessionBusy(sessionID)
	}, 30*time.Second, 20*time.Millisecond,
		"the root's own tool-use loop must finish before we cancel (see TIMING NOTE)")
	require.False(t, childHandlerUnblocked.Load(),
		"the child must still be blocked once the root's own turn has finished")

	// The external safety valve: --timeout/Ctrl-C cancel exactly this ctx
	// (internal/cmd/run.go), nothing more.
	cancel()

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("RunNonInteractiveWithResult did not return after its ctx was canceled")
	}
	require.Error(t, runErr)
	require.NotNil(t, result)
	require.Equal(t, "canceled", result.ExitReason)

	// THE LAW UNDER TEST, first half: a plain ctx cancellation of the root
	// run does NOT, by itself, stop the child's live turn — the delegated
	// tool's job context is deliberately detached (context.WithoutCancel,
	// internal/agent/async_tool.go:55), so an in-flight delegation survives
	// a turn-level cancellation. This is what makes "wait for live
	// descendant work" (ASYNC-02) correct rather than a leak; it also means
	// the scope is not yet torn down here.
	require.False(t, childHandlerUnblocked.Load(),
		"ctx cancellation alone must not have stopped the still-detached child turn")

	// Second half, and what a real `rush run` does next: cmd/run.go's
	// `defer a.Shutdown()` calls CancelAgents -> AgentCoordinator.CancelAll,
	// which sweeps EVERY mailbox (root and every descendant) unconditionally
	// (agent_control.go's CancelAll). That is the mechanism that actually
	// closes the scope down to nothing outstanding.
	application.AgentCoordinator.CancelAll()

	require.Eventually(t, func() bool {
		return childHandlerUnblocked.Load()
	}, 10*time.Second, 50*time.Millisecond,
		"CancelAll must stop the child's live turn (its blocked provider request must observe context cancellation)")
}
