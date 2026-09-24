package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestRunNonInteractiveWaitsForAsyncCommandAndReturnsOneFinalJSON(t *testing.T) {
	var requests atomic.Int32
	completionSeen := make(chan string, 1)
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
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
	require.EqualValues(t, 3, requests.Load())
	require.Equal(t, 1, strings.Count(strings.TrimSpace(stdout.String()), `"session_id"`))
	var wire RunResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &wire))
	require.Equal(t, "final answer", wire.FinalText)
	select {
	case body := <-completionSeen:
		require.Contains(t, body, "Async job call-async")
		require.Contains(t, body, "go version")
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
	require.Equal(t, 1, notices)
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
	require.EqualValues(t, 3, requests.Load())
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
		require.Contains(t, body, "Async job call-web")
	case <-time.After(10 * time.Second):
		t.Fatal("web agent did not resume after command completion")
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
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
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
	require.EqualValues(t, 1, parentStarted.Load())
	require.EqualValues(t, 1, parentResumed.Load())
	require.Equal(t, 1, strings.Count(output.String(), `"session_id"`))
	messages, err := application.Messages.List(t.Context(), sessionID)
	require.NoError(t, err)
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
	var requests atomic.Int32
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		switch {
		case n <= 11 && n%2 == 1:
			id := fmt.Sprintf("call-%d", n)
			admissionWriteSSE(w, []string{
				admissionSSEToolCall(fmt.Sprintf("start-%d", n), id, "run_command", `{"program":"go","args":["version"]}`),
				admissionSSEStop(fmt.Sprintf("start-%d", n), "tool_calls"),
			})
		case n <= 12:
			admissionWriteSSE(w, []string{admissionSSEText(fmt.Sprintf("waiting-%d", n), "waiting"), admissionSSEStop(fmt.Sprintf("waiting-%d", n), "stop")})
		case n == 13:
			admissionWriteSSE(w, []string{admissionSSEText("finished", "all six jobs finished"), admissionSSEStop("finished", "stop")})
		default:
			http.Error(w, "unexpected model call", http.StatusBadRequest)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var output bytes.Buffer
	result, err := application.RunNonInteractiveWithResult(ctx, &output, "run six jobs", RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.Equal(t, "all six jobs finished", result.FinalText)
	require.EqualValues(t, 13, requests.Load())
	require.Equal(t, 1, strings.Count(output.String(), `"session_id"`))
	messages, err := application.Messages.List(t.Context(), sessionID)
	require.NoError(t, err)
	var notices int
	for _, item := range messages {
		if item.BackgroundJobNotice {
			notices++
		}
	}
	require.Equal(t, 6, notices)
}
