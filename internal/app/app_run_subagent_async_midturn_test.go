package app

import (
	"context"
	"io"
	"net/http"
	"strings"
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
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		system, lastUser, lastTool := lastTurnParts(body)
		switch routeBatchModelRequest(body, commandRoot.Load().(string), delegateRoot.Load().(string)) {
		case "title":
			admissionWriteSSE(w, []string{admissionSSEText("title", "Child Result"), admissionSSEStop("title", "stop")})
		case "root:delegate":
			admissionWriteSSE(w, []string{admissionSSEToolCall("delegate", "call-agent", "agent", `{"prompt":"WORKER-MARKER build it"}`), admissionSSEStop("delegate", "tool_calls")})
		case "child:bash":
			var id string
			_ = application.DB().QueryRowContext(r.Context(), `SELECT child_session_id FROM batch_nodes WHERE owner_session_id = ? AND tool_call_id = 'call-agent' AND node_id = root_id ORDER BY rowid DESC LIMIT 1`, sessionID).Scan(&id)
			childID.Store(id)
			admissionWriteSSE(w, []string{admissionSSEToolCall("child-bash", "call-bash", "bash", `{"command":`+jsonString(gated)+`,"description":"build"}`), admissionSSEStop("child-bash", "tool_calls")})
		case "child:started":
			id := batchStartedRootID(lastTool)
			require.NotEmpty(t, id)
			commandRoot.Store(id)
			// HOLD this provider step: the child's turn is in flight
			// while the command finishes outside.
			close(holdEntered)
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
			err := application.DB().QueryRowContext(r.Context(), `SELECT state FROM batch_nodes WHERE owner_session_id = ? AND root_id = ? AND task_kind = 'shell'`, childID.Load().(string), commandRoot.Load().(string)).Scan(&state)
			if err != nil || state != "completed" {
				http.Error(w, "child completion notice before terminal completed row: "+state, http.StatusServiceUnavailable)
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("child-result", "child done: build output received "+marker), admissionSSEStop("child-result", "stop")})
		case "root:yield":
			id := batchStartedRootID(lastTool)
			require.NotEmpty(t, id)
			delegateRoot.Store(id)
			admissionWriteSSE(w, []string{admissionSSEText("root-yield", "root waiting"), admissionSSEStop("root-yield", "stop")})
		case "root:final":
			// The parent's delegation notice carries the child's answer;
			// the root answers only after the child's POST-result turn.
			rootFinal = lastUser
			if strings.Contains(lastUser, "child done") && strings.Contains(lastUser, marker) {
				rootFinalSeen.Store(true)
			}
			var state string
			err := application.DB().QueryRowContext(r.Context(), `SELECT state FROM batch_nodes WHERE owner_session_id = ? AND root_id = ? AND task_kind = 'shell'`, childID.Load().(string), commandRoot.Load().(string)).Scan(&state)
			if err != nil || state != "completed" {
				http.Error(w, "parent final before the child command completed: "+state, http.StatusServiceUnavailable)
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("root-final", "root done"), admissionSSEStop("root-final", "stop")})
		default:
			http.Error(w, "unexpected batch midturn route system="+system+" lastUser="+lastUser+" lastTool="+lastTool, http.StatusBadRequest)
		}
	})
	grantChildBackgroundShellTools(t, application)

	// Release the command exactly once the child's provider step is HELD:
	// entered -> release -> open. Deterministic, no sleeps.
	go func() {
		select {
		case <-holdEntered:
		case <-time.After(30 * time.Second):
			close(holdOpen)
			return
		}
		releaseRecoveryJob(t, release)
		close(holdOpen)
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	result, err := application.RunNonInteractiveWithResult(ctx, io.Discard, "delegate a build", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.NotEmpty(t, childID.Load().(string))
	require.NotEmpty(t, commandRoot.Load().(string))
	require.True(t, childSawMarker.Load(),
		"the child's completion notice must carry the command's actual marker output")
	require.True(t, rootFinalSeen.Load(),
		"the parent must receive the child's POST-result answer carrying the output: %q", rootFinal)
	require.Contains(t, rootFinal, "child done",
		"the parent must not be told the child's pre-result text: %q", rootFinal)
}
