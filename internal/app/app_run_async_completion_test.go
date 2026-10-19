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

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestRunNonInteractiveWaitsForAsyncCommandAndReturnsOneFinalJSON(t *testing.T) {
	var requests atomic.Int32
	completionSeen := make(chan string, 1)
	// Batch runtime: run_command is a batch work tool with SYNC delivery and
	// no background escape, so the command executes inline inside the first
	// turn and the SECOND request already carries its result -- the detached
	// announce/wait/complete flow (requests 2 and 3 below) is legacy-only.
	batch := config.BatchRuntimeEnabled()
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if batch {
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
			return
		}
		switch requests.Add(1) {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("async-start", "call-async", "run_command", `{"program":"go","args":["version"],"description":"check version"}`),
				admissionSSEStop("async-start", "tool_calls"),
			})
		case 2:
			admissionWriteSSE(w, []string{admissionSSEText("initial", "initial answer"), admissionSSEStop("initial", "stop")})
		case 3:
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
	if batch {
		require.EqualValues(t, 2, requests.Load(),
			"sync batch delivery: the command result is inline in the second request")
	} else {
		require.EqualValues(t, 3, requests.Load())
	}
	require.Equal(t, 1, strings.Count(strings.TrimSpace(stdout.String()), `"session_id"`))
	var wire RunResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &wire))
	require.Equal(t, "final answer", wire.FinalText)
	select {
	case body := <-completionSeen:
		require.Contains(t, body, "go version", "the command output must reach the model")
		if !batch {
			require.Contains(t, body, "Async job call-async")
		}
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
	if batch {
		require.Equal(t, 0, notices,
			"sync batch delivery completes inline: no model-visible completion notice")
	} else {
		require.Equal(t, 1, notices)
	}
}

func TestRunNonInteractiveDefaultCLIModeWaitsForAsyncCommand(t *testing.T) {
	var requests atomic.Int32
	batch := config.BatchRuntimeEnabled()
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		if batch {
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
			return
		}
		switch requests.Add(1) {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("cli-start", "call-cli", "run_command", `{"program":"go","args":["version"]}`),
				admissionSSEStop("cli-start", "tool_calls"),
			})
		case 2:
			admissionWriteSSE(w, []string{admissionSSEText("cli-initial", "job launched"), admissionSSEStop("cli-initial", "stop")})
		case 3:
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "Async job call-cli") {
				http.Error(w, "missing completion notice", http.StatusBadRequest)
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
	if batch {
		require.EqualValues(t, 2, requests.Load(),
			"sync batch delivery: the command result is inline in the second request")
	} else {
		require.EqualValues(t, 3, requests.Load())
	}
	require.Contains(t, output.String(), "work complete")
	require.NotContains(t, output.String(), "job launched")
}

func TestWebAsyncCommandCompletionCreatesModelVisibleNotice(t *testing.T) {
	var requests atomic.Int32
	completionSeen := make(chan string, 1)
	batch := config.BatchRuntimeEnabled()
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if batch {
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
			return
		}
		switch requests.Add(1) {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("web-start", "call-web", "run_command", `{"program":"go","args":["version"]}`),
				admissionSSEStop("web-start", "tool_calls"),
			})
		case 2:
			admissionWriteSSE(w, []string{admissionSSEText("web-initial", "started"), admissionSSEStop("web-initial", "stop")})
		case 3:
			completionSeen <- string(body)
			admissionWriteSSE(w, []string{admissionSSEText("web-final", "saw completion"), admissionSSEStop("web-final", "stop")})
		default:
			http.Error(w, "unexpected model call", http.StatusBadRequest)
		}
	})
	noticeCtx, stopNotices := context.WithCancel(t.Context())
	defer stopNotices()
	notices := application.Messages.Subscribe(noticeCtx)
	result, err := application.ExecuteRun(t.Context(), RunRequest{
		Prompt: "start web command", Mode: RunModeJSON, ContinueSessionID: sessionID,
		Origin: message.OriginWeb, Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	select {
	case body := <-completionSeen:
		require.Contains(t, body, "go version", "the command output must reach the model")
		if !batch {
			require.Contains(t, body, "Async job call-web")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("web agent did not resume after command completion")
	}
	if batch {
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
		return
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-notices:
			if event.Payload.SessionID != sessionID || !event.Payload.BackgroundJobNotice {
				continue
			}
			require.True(t, event.Payload.AutoResumed)
			persisted, getErr := application.Messages.Get(t.Context(), event.Payload.ID)
			require.NoError(t, getErr)
			require.True(t, persisted.BackgroundJobNotice)
			return
		case <-timer.C:
			t.Fatal("web completion notice was not persisted")
		}
	}
}

func TestRunNonInteractiveWaitsForAsyncSubAgentResult(t *testing.T) {
	var childCalls atomic.Int32
	var parentStarted atomic.Int32
	var parentResumed atomic.Int32
	batch := config.BatchRuntimeEnabled()
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case batch && strings.Contains(string(body), "child completed"):
			// Batch runtime: the agent tool runs the child INLINE, so the
			// parent's second request carries the child's result as the tool
			// result -- there is no async announcement and no completion
			// notice, the result itself resumes the parent.
			parentResumed.Add(1)
			admissionWriteSSE(w, []string{admissionSSEText("parent-final", "parent final"), admissionSSEStop("parent-final", "stop")})
		case strings.Contains(string(body), "Async job call-agent (agent) finished"):
			parentResumed.Add(1)
			admissionWriteSSE(w, []string{admissionSSEText("parent-final", "parent final"), admissionSSEStop("parent-final", "stop")})
		case strings.Contains(string(body), "Async agent job call-agent started"):
			parentStarted.Add(1)
			admissionWriteSSE(w, []string{admissionSSEText("parent-initial", "agent launched"), admissionSSEStop("parent-initial", "stop")})
		case strings.Contains(string(body), "WORKER-MARKER"):
			childCalls.Add(1)
			admissionWriteSSE(w, []string{admissionSSEText("child", "child completed"), admissionSSEStop("child", "stop")})
		default:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("delegate", "call-agent", "agent", `{"prompt":"WORKER-MARKER do the task"}`),
				admissionSSEStop("delegate", "tool_calls"),
			})
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var output bytes.Buffer
	result, err := application.RunNonInteractiveWithResult(ctx, &output, "start a worker", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "parent final", result.FinalText)
	require.GreaterOrEqual(t, childCalls.Load(), int32(1))
	require.EqualValues(t, 1, parentResumed.Load())
	if batch {
		require.EqualValues(t, 0, parentStarted.Load(),
			"sync batch delivery never announces an async agent job")
	} else {
		require.EqualValues(t, 1, parentStarted.Load())
	}
	require.Equal(t, 1, strings.Count(output.String(), `"session_id"`))
	messages, err := application.Messages.List(t.Context(), sessionID)
	require.NoError(t, err)
	if batch {
		for _, item := range messages {
			require.False(t, item.BackgroundJobNotice,
				"sync batch delivery: the child result is a tool result, not a notice")
		}
		return
	}
	var foundNotice bool
	for _, item := range messages {
		if item.BackgroundJobNotice {
			foundNotice = true
			require.Contains(t, item.FullText(), "child completed")
		}
	}
	require.True(t, foundNotice)
}

func TestRunNonInteractiveContinuesPastFiveAsyncCompletions(t *testing.T) {
	var mu sync.Mutex
	turnCount, launched := 0, 0
	barriers := make([]chan struct{}, 6)
	released := make([]bool, 6)
	seen := make(map[int]bool)
	for i := range barriers {
		barriers[i] = make(chan struct{})
	}
	oldTurnDone := cliLoopTurnDoneSeam
	cliLoopTurnDoneSeam = func() {
		mu.Lock()
		turnCount++
		if launched > 0 && !released[launched-1] {
			close(barriers[launched-1])
			released[launched-1] = true
		}
		mu.Unlock()
	}
	t.Cleanup(func() { cliLoopTurnDoneSeam = oldTurnDone })
	batch := config.BatchRuntimeEnabled()
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, lastUser, _ := lastTurnParts(body)
		for i := range barriers {
			if strings.TrimSpace(lastUser) == fmt.Sprintf("CHILD-WORK-%d", i+1) {
				if batch {
					// Batch runtime: the agent tool runs the child INLINE
					// inside the launching turn, so there is no detached
					// delivery to gate on the parent's turn boundary.
					admissionWriteSSE(w, []string{admissionSSEText(fmt.Sprintf("child-%d", i+1), fmt.Sprintf("child %d complete", i+1)), admissionSSEStop(fmt.Sprintf("child-%d", i+1), "stop")})
					return
				}
				select {
				case <-barriers[i]:
				case <-r.Context().Done():
					return
				case <-t.Context().Done():
					return
				}
				admissionWriteSSE(w, []string{admissionSSEText(fmt.Sprintf("child-%d", i+1), fmt.Sprintf("child %d complete", i+1)), admissionSSEStop(fmt.Sprintf("child-%d", i+1), "stop")})
				return
			}
		}
		mu.Lock()
		defer mu.Unlock()
		for i := 0; i < launched; i++ {
			if batch {
				// Inline children surface as tool results in the NEXT parent
				// request; there is no "Async job call-N" announcement.
				if strings.Contains(string(body), fmt.Sprintf("child %d complete", i+1)) {
					seen[i] = true
				}
				continue
			}
			if strings.Contains(string(body), fmt.Sprintf("Async job call-%d", i+1)) {
				seen[i] = true
			}
		}
		if batch {
			// Each parent turn launches exactly the next child; the results
			// accumulate one per parent request.
			if launched < len(barriers) {
				launched++
				admissionWriteSSE(w, []string{
					admissionSSEToolCall(fmt.Sprintf("start-%d", launched), fmt.Sprintf("call-%d", launched), "agent", fmt.Sprintf(`{"prompt":"CHILD-WORK-%d"}`, launched)),
					admissionSSEStop(fmt.Sprintf("start-%d", launched), "tool_calls"),
				})
				return
			}
		} else if launched < len(barriers) && (launched == 0 || seen[launched-1] && turnCount >= launched) {
			launched++
			admissionWriteSSE(w, []string{
				admissionSSEToolCall(fmt.Sprintf("start-%d", launched), fmt.Sprintf("call-%d", launched), "agent", fmt.Sprintf(`{"prompt":"CHILD-WORK-%d"}`, launched)),
				admissionSSEStop(fmt.Sprintf("start-%d", launched), "tool_calls"),
			})
			return
		}
		if launched == len(barriers) {
			all := true
			for i := range barriers {
				all = all && seen[i]
			}
			if all {
				admissionWriteSSE(w, []string{admissionSSEText("finished", "all six jobs finished"), admissionSSEStop("finished", "stop")})
				return
			}
		}
		admissionWriteSSE(w, []string{admissionSSEText(fmt.Sprintf("parent-%d", turnCount), "waiting"), admissionSSEStop(fmt.Sprintf("parent-%d", turnCount), "stop")})
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var output bytes.Buffer
	result, err := application.RunNonInteractiveWithResult(ctx, &output, "run six jobs", RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.Equal(t, "all six jobs finished", result.FinalText)
	mu.Lock()
	gotLaunched, gotTurns, gotSeen := launched, turnCount, len(seen)
	mu.Unlock()
	require.Equal(t, 6, gotLaunched)
	require.Equal(t, 7, gotTurns)
	require.Equal(t, 6, gotSeen)
	if batch {
		// Inline children never park behind a parent turn boundary: the
		// barrier mechanism is the detached-delivery oracle.
	} else {
		for _, barrier := range barriers {
			select {
			case <-barrier:
			default:
				t.Fatal("each child barrier must be released by its parent turn boundary")
			}
		}
	}
	require.Equal(t, 1, strings.Count(output.String(), `"session_id"`))
	messages, err := application.Messages.List(t.Context(), sessionID)
	require.NoError(t, err)
	var notices int
	for _, item := range messages {
		if item.BackgroundJobNotice {
			notices++
		}
	}
	if batch {
		require.Equal(t, 0, notices,
			"sync batch delivery completes inline: no completion notices accumulate")
	} else {
		require.Equal(t, 6, notices)
	}
}
