package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// Regression: in a CLI run a delegated sub-agent's own async bash result
// must reach that sub-agent as a new turn, as the async tool promises
// ("its result will arrive as a new session message"). The child's CLI
// completion used to land on the child session's ready queue, which only
// the ROOT loop drains, so the child never saw its command's output and the
// parent was told the delegation finished with the child's pre-result text.
func TestRunNonInteractiveChildReceivesItsOwnAsyncBashResult(t *testing.T) {
	if f6BatchOn() {
		testRunNonInteractiveChildReceivesItsOwnBatchBashResult(t)
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

// lastTurnParts returns the system prompt and the newest user and tool
// message texts of a provider request.
func lastTurnParts(body []byte) (system, lastUser, lastTool string) {
	var req wireRequest
	if json.Unmarshal(body, &req) != nil {
		return "", "", ""
	}
	for i, m := range req.Messages {
		s, _ := m.Content.(string)
		switch {
		case i == 0 && m.Role == "system":
			system = s
		case m.Role == "user":
			lastUser = s
		case m.Role == "tool":
			lastTool = s
		}
	}
	return system, lastUser, lastTool
}

// shortBuildCommand outlives bash's 1s fast-failure probe, then prints marker.
func shortBuildCommand(marker string) string {
	if runtime.GOOS == "windows" {
		return "ping -n 4 127.0.0.1 && echo " + marker
	}
	return "sleep 3 && echo " + marker
}

// testRunNonInteractiveChildReceivesItsOwnBatchBashResult is the flag-ON
// twin: the child's own bash command is a FOREGROUND call whose command
// blocks until released, so it provably outlives the real three-second
// inline window and the batch runtime converts it into a command root. The
// child must see the batch started response, consume the completion as a
// selected notice carrying the command's ACTUAL stdout, answer from that
// output, and the parent must only then be told the delegation finished
// with the child's POST-result answer.
func testRunNonInteractiveChildReceivesItsOwnBatchBashResult(t *testing.T) {
	const marker = "BUILD-MARKER-42"
	command, release := recoveryJobCommand(t)
	// The gated command prints the marker ONLY after the release sentinel
	// appears: the marker in the completion notice is proof the command
	// finished AFTER release, i.e. while the child's next provider step was
	// held inside the handler.
	gated := command + "; echo " + marker
	var commandRoot, delegateRoot, childID atomic.Value
	commandRoot.Store("")
	delegateRoot.Store("")
	childID.Store("")
	var childLaunches, completions, rootFinals atomic.Int32
	var childSawMarker, rootFinalSeen atomic.Bool
	rootFinal := ""
	// holdEntered is closed by the child's held post-started provider step;
	// holdOpen gates the handler's RESPONSE until the command has been
	// released -- deterministic ordering, zero timing sleeps.
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
			admissionWriteSSE(w, []string{admissionSSEToolCall("delegate", "call-agent", "agent", `{"prompt":"WORKER-MARKER do the task"}`), admissionSSEStop("delegate", "tool_calls")})
		case "child:bash":
			childLaunches.Add(1)
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
			// Foreground bash: the command outlives the real inline window,
			// so the batch runtime registers it as a command root.
			admissionWriteSSE(w, []string{admissionSSEToolCall("child-bash", "call-bash", "bash", `{"command":`+jsonString(gated)+`,"description":"build"}`), admissionSSEStop("child-bash", "tool_calls")})
		case "child:started":
			id := batchStartedRootID(lastTool)
			if id == "" {
				t.Errorf("child started tool result has no batch root id: %q", lastTool)
				http.Error(w, "no batch root id", http.StatusServiceUnavailable)
				return
			}
			commandRoot.Store(id)
			closeOnce(holdEnteredClose, holdEntered)
			select {
			case <-holdOpen:
			case <-time.After(30 * time.Second):
				http.Error(w, "hold gate never opened", http.StatusServiceUnavailable)
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("child-wait", "child waiting for build"), admissionSSEStop("child-wait", "stop")})
		case "child:final":
			// The selected completion notice; its outcome summary IS the
			// command's actual stdout, so the marker proves the real output
			// reached the child. The command root must already be terminal
			// "completed" and must own exactly ONE shell node (one command
			// execution).
			childSawMarker.Store(strings.Contains(lastUser, marker))
			var state string
			err := liveApp.Load().DB().QueryRowContext(r.Context(), `SELECT state FROM batch_nodes WHERE owner_session_id = ? AND root_id = ? AND task_kind = 'shell'`, childID.Load().(string), commandRoot.Load().(string)).Scan(&state)
			if err != nil || state != "completed" {
				http.Error(w, "child completion notice before terminal completed row: "+state, http.StatusServiceUnavailable)
				return
			}
			var shells int
			err = liveApp.Load().DB().QueryRowContext(r.Context(), `SELECT COUNT(*) FROM batch_nodes WHERE root_id = ? AND task_kind = 'shell'`, commandRoot.Load().(string)).Scan(&shells)
			if err != nil || shells != 1 {
				http.Error(w, "expected exactly one command shell node under the command root", http.StatusServiceUnavailable)
				return
			}
			// Count the completion only when the child's turn actually
			// succeeds: router admissions (including 403 retries) are not
			// deliveries.
			completions.Add(1)
			admissionWriteSSE(w, []string{admissionSSEText("child-result", "child done: build output received "+marker), admissionSSEStop("child-result", "stop")})
		case "child:wait":
			// A replayed started announcement (also persisted as user history):
			// the child just yields until its completion is pulled.
			admissionWriteSSE(w, []string{admissionSSEText("child-wait", "child waiting for build"), admissionSSEStop("child-wait", "stop")})
		case "root:wait":
			// A replayed started announcement on the root side.
			admissionWriteSSE(w, []string{admissionSSEText("root-wait", "root waiting"), admissionSSEStop("root-wait", "stop")})
		case "root:yield":
			id := batchStartedRootID(lastTool)
			if id == "" {
				t.Errorf("root started notice has no batch root id: %q", lastTool)
				http.Error(w, "no batch root id", http.StatusServiceUnavailable)
				return
			}
			delegateRoot.Store(id)
			admissionWriteSSE(w, []string{admissionSSEText("root-yield", "root waiting"), admissionSSEStop("root-yield", "stop")})
		case "root:final":
			// The parent's delegation notice carries the child's answer; the
			// root may only be resumed after the child's POST-result turn.
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
			rootFinals.Add(1)
			admissionWriteSSE(w, []string{admissionSSEText("root-final", "root done"), admissionSSEStop("root-final", "stop")})
		default:
			http.Error(w, "unexpected batch route system="+system+" lastUser="+lastUser+" lastTool="+lastTool, http.StatusBadRequest)
		}
	})
	liveApp.Store(application)
	liveSession.Store(sessionID)
	grantChildBackgroundShellTools(t, application)

	// Release the command exactly once the child's post-started provider
	// step is HELD: entered -> release -> gate. The command is released
	// FIRST, so it provably finishes while the step is held inside the
	// handler.
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

	require.EqualValues(t, 1, childLaunches.Load(), "the child must launch exactly one command")
	require.EqualValues(t, 1, completions.Load(), "the child must consume exactly one completion notice")
	require.EqualValues(t, 1, rootFinals.Load(), "the parent must be resumed exactly once")
	require.NotEmpty(t, childID.Load().(string))
	require.NotEmpty(t, commandRoot.Load().(string))
	require.True(t, childSawMarker.Load(),
		"the child's completion notice must carry the command's actual marker output")
	require.True(t, rootFinalSeen.Load(),
		"the parent must receive the child's POST-result answer carrying the output: %q", rootFinal)
	require.Contains(t, rootFinal, "child done",
		"the parent must not be told the child's pre-result text: %q", rootFinal)
	require.Equal(t, "root done", result.FinalText)
}
