package app

// TestBatchChildReactionScope_ParentSettlesOnlyAfterChildReaction proves
// (package S, jb/psc) that the flagged batch delegation stays settled-OPEN
// through the child's owed, in-flight and PACED completion reaction: an
// injected 403 paces the child's first reaction attempt, the parent must not
// be resumed by that failed attempt, and the parent finally receives the
// child's POST-result answer exactly once.
//
// DEPENDENCY (package W, jb/pw): the idle-child completion wake bridge —
// after the bash completion commits, the idle child must be woken to pull
// its selected notice. Without W's bridge the child never reacts and this
// test fails at root:final. This file implements none of W's files.

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

func TestBatchChildReactionScope_ParentSettlesOnlyAfterChildReaction(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "true")
	if !f6BatchOn() {
		t.Skip("flag-ON batch runtime fixture")
	}
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
	var childLaunches, completions, rootFinals, childFinalAttempts atomic.Int32
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
			// One-shot paced failure: the FIRST reaction attempt gets a 403,
			// which paces the child's debt. The parent must NOT settle on it.
			// The attempt is not counted as a delivery (no counter increment).
			if childFinalAttempts.Add(1) == 1 {
				http.Error(w, "injected 403: paced child reaction", http.StatusForbidden)
				return
			}
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

	require.GreaterOrEqual(t, childFinalAttempts.Load(), int32(2), "the injected 403 must have paced the child's first reaction attempt")
	require.EqualValues(t, 1, childLaunches.Load(), "the retry must reuse the same delegation and command: bash is never re-issued")
	require.EqualValues(t, 1, completions.Load(), "exactly one accepted child completion")
	require.EqualValues(t, 1, rootFinals.Load(), "the parent must be resumed exactly once")
	require.NotEmpty(t, childID.Load().(string))
	require.NotEmpty(t, commandRoot.Load().(string))
	require.True(t, childSawMarker.Load(),
		"the child's completion notice must carry the command's actual marker output")
	require.True(t, rootFinalSeen.Load(),
		"the parent must receive the child's POST-result answer: %q", rootFinal)
	require.Contains(t, rootFinal, "child done",
		"the parent must not be told the child's pre-result text: %q", rootFinal)
	require.Equal(t, "root done", result.FinalText)
}
