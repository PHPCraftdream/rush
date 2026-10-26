package app

// Flag-ON twins for the supervision acceptance fixture
// (app_run_supervision_test.go). Under the batch runtime, run_command is a
// batch work tool with SYNC delivery and escape never (batchFamilyDelivery
// / newBatchWorkToolBuild, internal/agent/batch_install_tools.go), so `go
// version` finishes inline inside the first turn and the SECOND provider
// request already carries its result -- the detached
// announce/wait/complete flow (requests 2 and 3 of the legacy fixture) is
// legacy-only. svRunSupervisionBatchSynchronousCommand keeps the prompt
// exit oracle for that synchronous shape; because an inline-answered
// command opens no detached work, the escaped-bash test below is the
// separately gated scenario that preserves the fixture's real acceptance
// property: work that DID escape (a sentinel-held bash command past the
// inline window, exactly the shape that arms supervision) must still let
// the run exit promptly -- nowhere near the 5-minute default supervision
// interval -- once that work completes and no other work is pending
// (docs/plans/2026-09-27-async-structured-concurrency.md §7). Whether
// supervision state is armed and cleared at the exact mechanism is
// covered by internal/agent's supervision suite; this stays the black-box
// confirmation at the layer `rush run` runs at.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// svRunSupervisionBatchSynchronousCommand is part (a), the synchronous
// run_command twin: the command result is the tool result of the second
// request and the run returns its final answer there -- no detached
// completion notice can follow. This alone proves prompt exit, NOT that
// supervision armed (an inline-answered call arms nothing); the escaped
// scenario below carries the arming-shaped property.
func svRunSupervisionBatchSynchronousCommand(t *testing.T) {
	t.Helper()
	var requests atomic.Int32
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch requests.Add(1) {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("sup-start", "call-sup", "run_command", `{"program":"go","args":["version"]}`),
				admissionSSEStop("sup-start", "tool_calls"),
			})
		case 2:
			if strings.Contains(string(body), "Async job call-sup") {
				t.Errorf("a synchronous batch run_command must not produce a detached completion notice")
				http.Error(w, "unexpected detached completion notice", http.StatusBadRequest)
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("sup-final", "work complete"), admissionSSEStop("sup-final", "stop")})
		default:
			http.Error(w, "unexpected model call", http.StatusBadRequest)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	result, err := application.RunNonInteractiveWithResult(ctx, &stdout, "start a background command", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err, "the run must not time out waiting on anything supervision-shaped")
	require.NotNil(t, result)
	require.Equal(t, "work complete", result.FinalText)
	require.EqualValues(t, 2, requests.Load(),
		"sync batch delivery: the command result is inline in the second request")
	require.Equal(t, 0, countBackgroundNotices(t, application, sessionID, ""),
		"the result traveled in the tool result; nothing detached for supervision to trail")

	var wire RunResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &wire))
	require.Equal(t, "work complete", wire.FinalText)
}

// TestRunNonInteractiveExitsPromptlyWithSupervisionArmedEscapedBashBatch is
// part (b), the separately gated escaped scenario: a sentinel-held bash
// command (recoveryJobCommand) escapes the inline window, so the run gets
// the batch started reaction turn and then drains -- the shape that arms
// supervision. The job is released by the provider handler the moment the
// durably persisted started text reaches the second request, so completion
// is deterministic; the run must exit promptly after exactly one selected
// completion, with no further work pending. The job's output content is
// deliberately NOT asserted here: the selected-notice result projection
// (review package N) is not merged yet, and asserting the marker now would
// bake in the known "(no output)" projection gap as an oracle.
func TestRunNonInteractiveExitsPromptlyWithSupervisionArmedEscapedBashBatch(t *testing.T) {
	if !f6BatchOn() {
		t.Skip("escaped-bash supervision shape is batch-runtime only; the legacy detached flow is covered by TestRunNonInteractiveExitsPromptlyWithSupervisionArmedAndNoOtherWork")
	}
	command, release := recoveryJobCommand(t)
	var releaseOnce sync.Once
	svRelease := func() {
		releaseOnce.Do(func() {
			if err := os.WriteFile(release, nil, 0o644); err != nil {
				t.Errorf("failed to release the held job: %v", err)
			}
		})
	}
	t.Cleanup(svRelease)

	var launches, startedReplies, completions atomic.Int32
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, lastUser, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(lastUser, "short title"):
			// Title generation must never consume a numeric execution
			// phase: routing is content-based for every request.
			admissionWriteSSE(w, []string{admissionSSEText("title", "Supervision"), admissionSSEStop("title", "stop")})
		case strings.Contains(lastUser, "Async job call-sup () finished"):
			completions.Add(1)
			admissionWriteSSE(w, []string{admissionSSEText("sup-final", "work complete"), admissionSSEStop("sup-final", "stop")})
		case strings.Contains(lastTool, "completions arrive as selected notices"):
			startedReplies.Add(1)
			svRelease()
			admissionWriteSSE(w, []string{admissionSSEText("sup-yield", "job started"), admissionSSEStop("sup-yield", "stop")})
		default:
			launches.Add(1)
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("sup-start", "call-sup", "bash",
					`{"command":`+jsonString(command)+`,"description":"held job"}`),
				admissionSSEStop("sup-start", "tool_calls"),
			})
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	result, err := application.RunNonInteractiveWithResult(ctx, &stdout, "start a background command", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err, "the run must exit promptly once the escaped job's work drains, not wait on anything supervision-shaped")
	require.NotNil(t, result)
	require.Equal(t, "work complete", result.FinalText)
	require.EqualValues(t, 1, launches.Load())
	require.EqualValues(t, 1, startedReplies.Load(),
		"one started reaction turn; the run must not demand a completion inline for an escaped job")
	require.EqualValues(t, 1, completions.Load(),
		"exactly one selected completion and no further work pending")

	var wire RunResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &wire))
	require.Equal(t, "work complete", wire.FinalText)
}
