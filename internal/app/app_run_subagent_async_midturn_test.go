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

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// Regression: the child's own async bash result must reach the child even
// when the job finishes while the child's delegated turn is still running.
func TestRunNonInteractiveChildReceivesAsyncBashResultFinishedMidTurn(t *testing.T) {
	if f6BatchOn() {
		testRunNonInteractiveChildReceivesBatchBashResultFinishedMidTurn(t)
		return
	}
	const marker = "BUILD-MARKER-42"
	var childSawResult atomic.Bool
	var rootFinal atomic.Value
	rootFinal.Store("")

	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		system, lastUser, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(system, "short title"):
			admissionWriteSSE(w, []string{admissionSSEText("title", "Child Result"), admissionSSEStop("title", "stop")})
		case strings.Contains(lastUser, "Async job call-agent (agent)"):
			rootFinal.Store(lastUser)
			admissionWriteSSE(w, []string{admissionSSEText("root-final", "root done"), admissionSSEStop("root-final", "stop")})
		case strings.Contains(lastUser, "Async job call-bash (bash)"):
			childSawResult.Store(strings.Contains(lastUser, marker))
			admissionWriteSSE(w, []string{admissionSSEText("child-result", "child done: build output received"), admissionSSEStop("child-result", "stop")})
		case strings.Contains(lastTool, "Async bash job"):
			time.Sleep(6 * time.Second) // the child's turn outlives its own bash job
			admissionWriteSSE(w, []string{admissionSSEText("child-wait", "child waiting for build"), admissionSSEStop("child-wait", "stop")})
		case strings.Contains(lastUser, "WORKER-MARKER"):
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("child-bash", "call-bash", "bash",
					`{"command":`+jsonString(shortBuildCommand(marker))+`,"description":"build"}`),
				admissionSSEStop("child-bash", "tool_calls"),
			})
		case strings.Contains(lastTool, "Async agent job call-agent started"):
			admissionWriteSSE(w, []string{admissionSSEText("root-yield", "root waiting"), admissionSSEStop("root-yield", "stop")})
		default:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("delegate", "call-agent", "agent", `{"prompt":"WORKER-MARKER build it"}`),
				admissionSSEStop("delegate", "tool_calls"),
			})
		}
	})
	grantChildBackgroundShellTools(t, application)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	result, err := application.RunNonInteractiveWithResult(ctx, io.Discard, "delegate a build", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.True(t, childSawResult.Load(),
		"the sub-agent never received its own bash result; parent was told: %q", rootFinal.Load())
	require.Contains(t, rootFinal.Load().(string), "child done",
		"the parent must receive the child's post-result answer, not its pre-result text")
}

// testRunNonInteractiveChildReceivesBatchBashResultFinishedMidTurn is the
// flag-ON twin (review G2#11): the child's bash call escapes the real
// three-second window and returns the batch started text; the child's
// SUBSEQUENT provider step is then HELD inside the handler while the command
// is released and finishes, proving the completion landed mid-turn. The
// selected completion notice must carry the command's ACTUAL stdout (the
// batchNoticeText outcome summary is the leaf response content), the child
// must answer with that output, and the parent's delegation notice must
// carry the child's POST-result answer.
func testRunNonInteractiveChildReceivesBatchBashResultFinishedMidTurn(t *testing.T) {
	const marker = "BUILD-MARKER-42"
	command, release := recoveryJobCommand(t)
	// The gated command prints the marker ONLY after the release sentinel
	// appears: the marker in the completion notice is therefore proof the
	// command finished AFTER release, i.e. mid-turn while the child's next
	// provider step was held.
	gated := command + "; echo " + marker
	var commandRoot, delegateRoot, childID atomic.Value
	commandRoot.Store("")
	delegateRoot.Store("")
	childID.Store("")
	var childSawMarker, rootFinalSeen atomic.Bool
	rootFinal := ""
	// holdEntered is closed by the child's held provider step; holdOpen
	// gates the handler's RESPONSE until the test goroutine has released
	// the command -- so the command provably finishes while the step is in
	// flight, with zero timing sleeps.
	holdEntered := make(chan struct{})
	holdOpen := make(chan struct{})
	holdEnteredClose := &sync.Once{}
	holdOpenClose := &sync.Once{}
	releaseOnce := &sync.Once{}
	releaseErr := make(chan error, 1)
	helperCtx, cancelHelper := context.WithCancel(context.Background())
	var liveApp atomic.Pointer[App]
	var liveSession atomic.Value
	router := newRBRouter()
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		system, lastUser, lastTool := lastTurnParts(body)
		route, seqErr := router.route(body)
		if seqErr != "" {
			http.Error(w, "out-of-sequence batch request: "+seqErr, http.StatusBadRequest)
			return
		}
		switch route {
		case "title":
			admissionWriteSSE(w, []string{admissionSSEText("title", "Child Result"), admissionSSEStop("title", "stop")})
		case "root:delegate":
			admissionWriteSSE(w, []string{admissionSSEToolCall("delegate", "call-agent", "agent", `{"prompt":"WORKER-MARKER build it"}`), admissionSSEStop("delegate", "tool_calls")})
		case "child:bash":
			var id string
			err := liveApp.Load().DB().QueryRowContext(r.Context(), `
SELECT t.child_session_id
FROM batch_nodes r
JOIN batch_nodes t
  ON t.owner_session_id = r.owner_session_id
 AND t.root_id = r.root_id AND t.host_id = r.host_id
 AND t.kind = 'task'
WHERE r.owner_session_id = ? AND r.tool_call_id = 'call-agent'
  AND r.node_id = r.root_id AND r.kind = 'batch'
ORDER BY t.ordinal LIMIT 1`, liveSession.Load().(string)).Scan(&id)
			if err != nil {
				http.Error(w, "delegation child lookup failed: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
			if id == "" {
				http.Error(w, "delegation child lookup returned an empty child id", http.StatusServiceUnavailable)
				return
			}
			childID.Store(id)
			admissionWriteSSE(w, []string{admissionSSEToolCall("child-bash", "call-bash", "bash", `{"command":`+jsonString(gated)+`,"description":"build"}`), admissionSSEStop("child-bash", "tool_calls")})
		case "child:started":
			id := batchStartedRootID(lastTool)
			if id == "" {
				t.Errorf("child started tool result has no batch root id: %q", lastTool)
				http.Error(w, "no batch root id", http.StatusServiceUnavailable)
				return
			}
			commandRoot.Store(id)
			// HOLD this provider step: the child's turn is in flight
			// while the command finishes outside.
			closeOnce(holdEnteredClose, holdEntered)
			select {
			case <-holdOpen:
			case <-time.After(30 * time.Second):
				http.Error(w, "hold gate never opened", http.StatusServiceUnavailable)
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("child-wait", "child waiting for build"), admissionSSEStop("child-wait", "stop")})
		case "child:final":
			// The selected completion notice. Its outcome summary IS the
			// leaf's response content (batchNoticeText outcome =
			// OutcomeSummary = resp.Content), so the marker proves the
			// command's actual output reached the child; the DB row must
			// already be terminal "completed".
			childSawMarker.Store(strings.Contains(lastUser, marker))
			var state string
			err := liveApp.Load().DB().QueryRowContext(r.Context(), `SELECT state FROM batch_nodes WHERE owner_session_id = ? AND root_id = ? AND task_kind = 'shell'`, childID.Load().(string), commandRoot.Load().(string)).Scan(&state)
			if err != nil || state != "completed" {
				http.Error(w, "child completion notice before terminal completed row: "+state, http.StatusServiceUnavailable)
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("child-result", "child done: build output received "+marker), admissionSSEStop("child-result", "stop")})
		case "root:yield":
			id := batchStartedRootID(lastTool)
			if id == "" {
				t.Errorf("root started notice has no batch root id: %q", lastTool)
				http.Error(w, "no batch root id", http.StatusServiceUnavailable)
				return
			}
			delegateRoot.Store(id)
			admissionWriteSSE(w, []string{admissionSSEText("root-yield", "root waiting"), admissionSSEStop("root-yield", "stop")})
		case "child:wait":
			// A replayed started announcement (also persisted as user history):
			// the child just yields until its completion is pulled.
			admissionWriteSSE(w, []string{admissionSSEText("child-wait", "child waiting for build"), admissionSSEStop("child-wait", "stop")})
		case "root:wait":
			// A replayed started announcement on the root side.
			admissionWriteSSE(w, []string{admissionSSEText("root-wait", "root waiting"), admissionSSEStop("root-wait", "stop")})
		case "root:final":
			// The parent's delegation notice carries the child's answer;
			// the root answers only after the child's POST-result turn.
			rootFinal = lastUser
			if strings.Contains(lastUser, "child done") && strings.Contains(lastUser, marker) {
				rootFinalSeen.Store(true)
			}
			var state string
			err := liveApp.Load().DB().QueryRowContext(r.Context(), `SELECT state FROM batch_nodes WHERE owner_session_id = ? AND root_id = ? AND task_kind = 'shell'`, childID.Load().(string), commandRoot.Load().(string)).Scan(&state)
			if err != nil || state != "completed" {
				http.Error(w, "parent final before the child command completed: "+state, http.StatusServiceUnavailable)
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("root-final", "root done"), admissionSSEStop("root-final", "stop")})
		default:
			http.Error(w, "unexpected batch midturn route system="+pr2Excerpt(system)+" lastUser="+pr2Excerpt(lastUser)+" lastTool="+pr2Excerpt(lastTool), http.StatusBadRequest)
		}
	})
	liveApp.Store(application)
	liveSession.Store(sessionID)
	grantChildBackgroundShellTools(t, application)

	// Release the command exactly once the child's provider step is HELD:
	// entered -> release -> open. Deterministic, no sleeps. The command is
	// released FIRST, then the gate is opened, so the command provably
	// finishes while the step is still held inside the handler.
	holdHeld := make(chan struct{})
	helperDone := make(chan struct{})
	go func() {
		defer close(helperDone)
		select {
		case <-holdEntered:
		case <-helperCtx.Done():
			return
		case <-time.After(30 * time.Second):
			// The handler's own 30s hold refusal fails the run.
			return
		}
		close(holdHeld)
		// command first, then the gate: the command provably finishes
		// while the step is held
		if err := releaseRecoveryJobAsync(release); err != nil {
			select {
			case releaseErr <- err:
			default:
			}
			closeOnce(holdOpenClose, holdOpen)
			return
		}
		// REAL mid-turn gate: hold the response closed until the command's
		// batch row is terminal, so "terminal while the step was held" is
		// observed, not assumed. The child's provider step stays blocked the
		// whole time; no sleep-based timing.
		if err := pr2AwaitShellTerminal(liveApp.Load(), childID.Load().(string), commandRoot.Load().(string), 30*time.Second); err != nil {
			t.Errorf("command never became terminal while the held step was still blocked: %v", err)
			closeOnce(holdOpenClose, holdOpen)
			return
		}
		closeOnce(holdOpenClose, holdOpen)
	}()
	// Teardown ordering: cancel the helper, open the gate (no-op if already
	// open), release the command (idempotent), then join the goroutine
	// boundedly and loudly. require and t.Errorf panic when reached on a
	// goroutine, so the join must happen before the test returns.
	t.Cleanup(func() {
		cancelHelper()
		closeOnce(holdOpenClose, holdOpen)
		releaseOnce.Do(func() { _ = releaseRecoveryJobAsync(release) })
		select {
		case <-helperDone:
		case <-time.After(35 * time.Second):
			t.Errorf("helper goroutine did not exit before teardown")
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	result, err := application.RunNonInteractiveWithResult(ctx, io.Discard, "delegate a build", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, result)

	held := false
	select {
	case <-holdHeld:
		held = true
	default:
	}
	require.True(t, held, "the child's post-started provider step was never held")
	select {
	case err := <-releaseErr:
		require.NoError(t, err, "helper failed to write the release sentinel")
	default:
	}

	require.NotEmpty(t, childID.Load().(string))
	require.NotEmpty(t, commandRoot.Load().(string))
	require.True(t, childSawMarker.Load(),
		"the child's completion notice must carry the command's actual marker output")
	require.True(t, rootFinalSeen.Load(),
		"the parent must receive the child's POST-result answer carrying the output: %q", rootFinal)
	require.Contains(t, rootFinal, "child done",
		"the parent must not be told the child's pre-result text: %q", rootFinal)
}
