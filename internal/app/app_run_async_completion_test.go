package app

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestRunNonInteractiveWaitsForAsyncCommandAndReturnsOneFinalJSON(t *testing.T) {
	var requests atomic.Int32
	completionSeen := make(chan string, 1)
	// Batch runtime: run_command is a batch work tool with SYNC delivery and
	// no background escape, so the command executes inline inside the first
	// turn and the SECOND request already carries its result.
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch requests.Add(1) {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("async-start", "call-async", "run_command", `{"program":"go","args":["version"],"description":"check version"}`),
				admissionSSEStop("async-start", "tool_calls"),
			})
		case 2:
			completionSeen <- string(body)
			admissionWriteSSE(w, []string{admissionSSEText("final", "final answer"), admissionSSEStop("final", "stop")})
		default:
			http.Error(w, "unexpected model call", http.StatusBadRequest)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	result, err := application.RunNonInteractiveWithResult(ctx, &stdout, "start async command", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "final answer", result.FinalText)
	require.EqualValues(t, 2, requests.Load(),
		"sync batch delivery: the command result is inline in the second request")
	require.Equal(t, 1, strings.Count(strings.TrimSpace(stdout.String()), `"session_id"`))
	var wire RunResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &wire))
	require.Equal(t, "final answer", wire.FinalText)
	select {
	case body := <-completionSeen:
		require.Contains(t, body, "go version", "the command output must reach the model")
	default:
		t.Fatal("the final model turn did not receive the completion message")
	}
	messages, err := application.Messages.List(context.Background(), sessionID)
	require.NoError(t, err)
	var notices int
	for _, item := range messages {
		if item.BackgroundJobNotice {
			notices++
			require.Equal(t, message.User, item.Role)
		}
	}
	require.Equal(t, 0, notices,
		"sync batch delivery completes inline: no model-visible completion notice")
}

func TestRunNonInteractiveDefaultCLIModeWaitsForAsyncCommand(t *testing.T) {
	var requests atomic.Int32
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("cli-start", "call-cli", "run_command", `{"program":"go","args":["version"]}`),
				admissionSSEStop("cli-start", "tool_calls"),
			})
		case 2:
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "go version") {
				http.Error(w, "missing inline command result", http.StatusBadRequest)
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("cli-final", "work complete"), admissionSSEStop("cli-final", "stop")})
		default:
			http.Error(w, "unexpected model call", http.StatusBadRequest)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var output bytes.Buffer
	result, err := application.RunNonInteractiveWithResult(ctx, &output, "run from CLI", RunOverrides{Origin: message.OriginCLI}, true, RunModeTerse, sessionID, false)
	require.NoError(t, err)
	require.Equal(t, "work complete", result.FinalText)
	require.EqualValues(t, 2, requests.Load(),
		"sync batch delivery: the command result is inline in the second request")
	require.Contains(t, output.String(), "work complete")
	require.NotContains(t, output.String(), "job launched")
}

func TestWebAsyncCommandCompletionCreatesModelVisibleNotice(t *testing.T) {
	var requests atomic.Int32
	completionSeen := make(chan string, 1)
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch requests.Add(1) {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("web-start", "call-web", "run_command", `{"program":"go","args":["version"]}`),
				admissionSSEStop("web-start", "tool_calls"),
			})
		case 2:
			completionSeen <- string(body)
			admissionWriteSSE(w, []string{admissionSSEText("web-final", "saw completion"), admissionSSEStop("web-final", "stop")})
		default:
			http.Error(w, "unexpected model call", http.StatusBadRequest)
		}
	})
	result, err := application.ExecuteRun(t.Context(), RunRequest{
		Prompt: "start web command", Mode: RunModeJSON, ContinueSessionID: sessionID,
		Origin: message.OriginWeb, Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	select {
	case body := <-completionSeen:
		require.Contains(t, body, "go version", "the command output must reach the model")
	case <-time.After(10 * time.Second):
		t.Fatal("web agent did not resume after command completion")
	}
	// Sync delivery: the command completed inline, nothing is resumed and
	// no completion notice exists to persist -- the run's final answer is
	// the whole observable outcome.
	require.Equal(t, "saw completion", result.FinalText)
	messages, listErr := application.Messages.List(context.Background(), sessionID)
	require.NoError(t, listErr)
	for _, item := range messages {
		require.False(t, item.BackgroundJobNotice,
			"sync batch delivery must not persist a background-job notice")
	}
}

// TestRunNonInteractiveWaitsForAsyncSubAgentResult: the delegated child runs
// to completion and the parent is resumed exactly once by its selected
// terminal completion, whose notice carries the child's actual text.
func TestRunNonInteractiveWaitsForAsyncSubAgentResult(t *testing.T) {
	started := make(chan struct{})
	gate := make(chan struct{})
	var turns atomic.Int32
	var launches atomic.Int32
	var startedReplies atomic.Int32
	var parentResumed atomic.Int32
	var releaseOnce sync.Once
	var releaseStarted sync.Once
	releaseChild := func() { releaseOnce.Do(func() { close(gate) }) }
	old := cliLoopTurnDoneSeam
	cliLoopTurnDoneSeam = func() {
		turns.Add(1)
		releaseStarted.Do(func() { close(started) })
		releaseChild()
	}
	t.Cleanup(func() { cliLoopTurnDoneSeam = old })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var childCalls atomic.Int32
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, lastUser, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(lastUser, "Generate a concise title"):
			// Title requests quote the original prompt (agent_title.go), so
			// the child's title carries WORKER-MARKER. Answer plainly and
			// count nothing: a title request is not a child execution.
			admissionWriteSSE(w, []string{admissionSSEText("title", "test title"), admissionSSEStop("title", "stop")})
		case strings.Contains(lastUser, "Async job call-agent () finished"):
			parentResumed.Add(1)
			admissionWriteSSE(w, []string{admissionSSEText("final", "parent final"), admissionSSEStop("final", "stop")})
		case strings.Contains(lastTool, "completions arrive as selected notices"):
			startedReplies.Add(1)
			admissionWriteSSE(w, []string{admissionSSEText("yield", "agent launched"), admissionSSEStop("yield", "stop")})
		case strings.Contains(lastUser, "WORKER-MARKER"):
			childCalls.Add(1)
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			case <-ctx.Done():
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("child", "child actual text"), admissionSSEStop("child", "stop")})
		default:
			launches.Add(1)
			admissionWriteSSE(w, []string{admissionSSEToolCall("delegate", "call-agent", "agent", `{"prompt":"WORKER-MARKER"}`), admissionSSEStop("delegate", "tool_calls")})
		}
	})
	t.Cleanup(func() { releaseChild(); application.Shutdown() })
	done := make(chan struct{})
	var result *RunResult
	var runErr error
	go func() {
		defer close(done)
		result, runErr = application.RunNonInteractiveWithResult(ctx, io.Discard, "start a worker", RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("parent turn did not yield")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("run did not complete")
	}
	require.NoError(t, runErr)
	require.NotNil(t, result)
	require.Equal(t, "parent final", result.FinalText)
	require.EqualValues(t, 1, childCalls.Load())
	require.EqualValues(t, 1, launches.Load())
	require.EqualValues(t, 1, startedReplies.Load())
	require.EqualValues(t, 1, parentResumed.Load())
	require.EqualValues(t, 2, turns.Load())
	// Only selected terminal completions carry NoticeKind "root"; the started
	// announcement is also a BackgroundJobNotice but has no kind.
	require.Equal(t, 1, countBackgroundNotices(t, application, sessionID, "root"))
	messages, err := application.Messages.List(t.Context(), sessionID)
	require.NoError(t, err)
	for _, item := range messages {
		if item.BackgroundJobNotice && item.NoticeKind == "root" {
			require.Contains(t, item.FullText(), "child actual text")
		}
	}
}

// TestRunNonInteractiveContinuesPastFiveAsyncCompletions: a CLI loop can chain
// more than five delegations -- each selected completion is consumed in order,
// launches exactly the next job, and the sixth completion ends the run.
func TestRunNonInteractiveContinuesPastFiveAsyncCompletions(t *testing.T) {
	var mu sync.Mutex
	turns, launched, consumed, started := 0, 0, 0, 0
	startedAcks := make(map[int]bool)
	barriers := make([]chan struct{}, 6)
	for i := range barriers {
		barriers[i] = make(chan struct{})
	}
	old := cliLoopTurnDoneSeam
	cliLoopTurnDoneSeam = func() {
		mu.Lock()
		turns++
		if launched > 0 {
			select {
			case <-barriers[launched-1]:
			default:
				close(barriers[launched-1])
			}
		}
		mu.Unlock()
	}
	t.Cleanup(func() { cliLoopTurnDoneSeam = old })
	var children atomic.Int32
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		event := pf2Classify(body)
		switch event.kind {
		case "title":
			// Title requests quote the run prompt (agent_title.go); answer
			// plainly and count nothing.
			admissionWriteSSE(w, []string{admissionSSEText("title", "test title"), admissionSSEStop("title", "stop")})
		case "child":
			mu.Lock()
			late := event.index > launched
			gotLaunched := launched
			mu.Unlock()
			if late {
				http.Error(w, fmt.Sprintf("child %d before its launch (launched=%d)", event.index, gotLaunched), http.StatusBadRequest)
				return
			}
			children.Add(1)
			select {
			case <-barriers[event.index-1]:
			case <-r.Context().Done():
				return
			case <-ctx.Done():
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText(fmt.Sprintf("child-%d", event.index), fmt.Sprintf("child %d actual", event.index)), admissionSSEStop(fmt.Sprintf("child-%d", event.index), "stop")})
		case "delegate":
			mu.Lock()
			if launched != 0 {
				gotLaunched := launched
				mu.Unlock()
				http.Error(w, fmt.Sprintf("duplicate initial delegation (launched=%d)", gotLaunched), http.StatusBadRequest)
				return
			}
			launched = 1
			mu.Unlock()
			admissionWriteSSE(w, []string{admissionSSEToolCall("start-1", "call-1", "agent", `{"prompt":"CHILD-WORK-1"}`), admissionSSEStop("start-1", "tool_calls")})
		case "started":
			idx := pf2CallIndex(event.callID)
			mu.Lock()
			reject := ""
			switch {
			case idx == 0:
				reject = "started tool result without a call-N id: " + event.detail
			case startedAcks[idx]:
				reject = fmt.Sprintf("duplicate started ack for %s", event.callID)
			case idx != launched:
				gotLaunched := launched
				reject = fmt.Sprintf("started for %s but launched=call-%d", event.callID, gotLaunched)
			default:
				startedAcks[idx] = true
				started++
			}
			mu.Unlock()
			if reject != "" {
				http.Error(w, "started-phase rejection: "+reject, http.StatusBadRequest)
				return
			}
			// The started tool result composes its own yield turn; only a NEW
			// selected completion advances the sequence.
			admissionWriteSSE(w, []string{admissionSSEText(fmt.Sprintf("yield-%d", started), "job started"), admissionSSEStop(fmt.Sprintf("yield-%d", started), "stop")})
		case "completion":
			idx := pf2CallIndex(event.callID)
			mu.Lock()
			reject := ""
			switch {
			case idx != consumed+1:
				gotConsumed, gotLaunched, gotStarted := consumed, launched, started
				reject = fmt.Sprintf("expected completion call-%d, found %s (consumed=%d, launched=%d, started=%d)", gotConsumed+1, event.callID, gotConsumed, gotLaunched, gotStarted)
			case !strings.Contains(event.text, fmt.Sprintf("child %d actual", idx)):
				reject = fmt.Sprintf("completion %s lost the child result (want %q)", event.callID, fmt.Sprintf("child %d actual", idx))
			default:
				consumed++
			}
			mu.Unlock()
			if reject != "" {
				http.Error(w, "completion-phase rejection: "+reject, http.StatusBadRequest)
				return
			}
			mu.Lock()
			if consumed == 6 {
				mu.Unlock()
				admissionWriteSSE(w, []string{admissionSSEText("finished", "all six jobs finished"), admissionSSEStop("finished", "stop")})
				return
			}
			launched++
			next := launched
			mu.Unlock()
			admissionWriteSSE(w, []string{admissionSSEToolCall(fmt.Sprintf("start-%d", next), fmt.Sprintf("call-%d", next), "agent", fmt.Sprintf(`{"prompt":"CHILD-WORK-%d"}`, next)), admissionSSEStop(fmt.Sprintf("start-%d", next), "tool_calls")})
		default:
			http.Error(w, "unexpected provider request: "+event.detail, http.StatusBadRequest)
		}
	})
	t.Cleanup(application.Shutdown)
	result, err := application.RunNonInteractiveWithResult(ctx, io.Discard, "run six jobs", RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "all six jobs finished", result.FinalText)
	mu.Lock()
	gotLaunch, gotStarted, gotConsumed, gotTurns := launched, started, consumed, turns
	mu.Unlock()
	// turns counts CLI LOOP turns (cliLoopTurnDoneSeam fires once per
	// ExecuteRun, app_run_async.go), not provider requests: the started
	// tool result and its yield text are steps of the SAME run as the launch.
	// 7 = the initial run (call-1 + yield) + one Drain per selected completion
	// (completions 1-5 each launch the next call and yield; completion 6 ends).
	require.Equal(t, 6, gotLaunch, fmt.Sprintf("launched=%d started=%d consumed=%d turns=%d", gotLaunch, gotStarted, gotConsumed, gotTurns))
	require.Equal(t, 6, gotStarted, fmt.Sprintf("launched=%d started=%d consumed=%d turns=%d", gotLaunch, gotStarted, gotConsumed, gotTurns))
	require.Equal(t, 6, gotConsumed, fmt.Sprintf("launched=%d started=%d consumed=%d turns=%d", gotLaunch, gotStarted, gotConsumed, gotTurns))
	require.Equal(t, 7, gotTurns, fmt.Sprintf("launched=%d started=%d consumed=%d turns=%d", gotLaunch, gotStarted, gotConsumed, gotTurns))
	require.EqualValues(t, 6, children.Load())
	messages, err := application.Messages.List(t.Context(), sessionID)
	require.NoError(t, err)
	counts := make(map[string]int)
	for _, item := range messages {
		// Started announcements carry no NoticeKind; count completions only.
		if item.BackgroundJobNotice && item.NoticeKind == "root" {
			for i := 1; i <= 6; i++ {
				id := fmt.Sprintf("call-%d", i)
				if strings.Contains(item.FullText(), id) {
					counts[id]++
					require.Contains(t, item.FullText(), fmt.Sprintf("child %d actual", i))
				}
			}
		}
	}
	for i := 1; i <= 6; i++ {
		require.Equal(t, 1, counts[fmt.Sprintf("call-%d", i)])
	}
}
