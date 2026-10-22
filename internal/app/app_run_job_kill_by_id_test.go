package app

// Regression for task #1053, batch runtime edition: in a CLI run the root
// must be able to stop its OWN long-running batch command using the root id
// it actually received from the escaped-window started text ("batch root
// <id> started; completions arrive as selected notices."). The legacy
// job_kill(job_id) transcript this file used to carry is deleted with the
// removed tool; the retained scenario is the batch_stop(root_id) twin below.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// grantRootBatchStop adds batch_stop to the ROOT (coder) agent's allowed
// tools. batch_allowed_controls.go grants batch_stop BY NAME from the
// allowed list (never via the removed job_kill tool), so without this the root's batch_stop
// call fails with "not allowed" and the ON stop scenario cannot run. This is
// harness configuration; the root agent is the CLI session's own agent, not
// the delegated task agent.
func grantRootBatchStop(t *testing.T, application *App) {
	t.Helper()
	cfg := application.config.Config()
	coder, ok := cfg.Agents[config.AgentCoder]
	require.True(t, ok, "the root coder agent must be configured")
	tools := slices.Clone(coder.AllowedTools)
	if !slices.Contains(tools, "batch_stop") {
		tools = append(tools, "batch_stop")
	}
	application.config.UpdateAgentAllowedTools(config.AgentCoder, tools)
}

// TestRunNonInteractiveRootKillsOwnBatchJobByStop: the
// ROOT's OWN bash call escapes the inline window and returns the batch
// started text ("batch root <id> started; completions arrive as selected
// notices."). The root extracts the advertised root id and issues batch_stop
// -- whose result is cancellation INTENT (accepted, not terminal) -- and the
// run may only finish after the selected terminal stopped completion
// (model-visible "Async job call-bash () was stopped (job_kill)"), with the
// cancelled state confirmed in the DB.
func TestRunNonInteractiveRootKillsOwnBatchJobByStop(t *testing.T) {
	command, release := recoveryJobCommand(t)
	defer releaseRecoveryJob(t, release)
	var extractedRootID atomic.Value
	extractedRootID.Store("")
	var stopIntentObserved atomic.Bool
	var terminalNoticeSeen atomic.Bool
	var liveApp atomic.Pointer[App]
	var liveSession atomic.Value
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		system, lastUser, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(system, "short title"):
			admissionWriteSSE(w, []string{admissionSSEText("title", "Job Kill By ID"), admissionSSEStop("title", "stop")})

		// The selected terminal notice arrives as the real model-visible
		// "Async job call-bash () was stopped (job_kill)" text
		// (FormatAsyncCompletion; Stopped is formatted before Cancelled, so
		// cancelled state is established by the DB check below, never by the
		// text). This is the CONFIRMED terminal event; only now may the run
		// finish. (Checking the DB row as well makes the ordering
		// production-proof, not stub-proof.)
		case strings.HasPrefix(lastUser, "Async job call-bash () was stopped (job_kill)"):
			var state string
			err := liveApp.Load().DB().QueryRowContext(r.Context(), `SELECT state FROM batch_nodes WHERE owner_session_id = ? AND root_id = ? AND task_kind = 'shell'`, liveSession.Load().(string), extractedRootID.Load().(string)).Scan(&state)
			if err != nil || state != "cancelled" {
				http.Error(w, "terminal notice before the command row was cancelled: "+state, http.StatusServiceUnavailable)
				return
			}
			terminalNoticeSeen.Store(true)
			// Same-turn deliveries carry the stop ack in history, so the
			// intent is provable without a dedicated ack turn.
			var histReq wireRequest
			if json.Unmarshal(body, &histReq) == nil {
				for _, m := range histReq.Messages {
					if m.Role != "tool" {
						continue
					}
					if s, ok := m.Content.(string); ok && strings.Contains(s, `"accepted":true`) && strings.Contains(s, `"terminal":false`) {
						stopIntentObserved.Store(true)
					}
				}
			}
			admissionWriteSSE(w, []string{admissionSSEText("final", "root final: batch job stopped"), admissionSSEStop("final", "stop")})

		// batch_stop's own response: accepted cancellation INTENT, never a
		// completion. Serve a yield; the run continues until the selected
		// terminal notice above.
		case strings.Contains(lastTool, `"accepted":true`) && strings.Contains(lastTool, `"terminal":false`):
			stopIntentObserved.Store(true)
			admissionWriteSSE(w, []string{admissionSSEText("stop-yield", "stop intent recorded; awaiting terminal confirmation"), admissionSSEStop("stop-yield", "stop")})

		// The bash call's escaped-window started result advertises the
		// batch root id. Extract it exactly as the model would and call
		// batch_stop with root_id (the id it actually received).
		case strings.Contains(lastTool, "batch root ") && strings.Contains(lastTool, "started; completions arrive as selected notices"):
			id := batchStartedRootID(lastTool)
			if id == "" {
				http.Error(w, "could not extract batch root id from: "+lastTool, http.StatusBadRequest)
				return
			}
			extractedRootID.Store(id)
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("stop", "call-stop", "batch_stop", `{"root_id":`+jsonString(id)+`}`),
				admissionSSEStop("stop", "tool_calls"),
			})

		// The started announcement is persisted as a user history message,
		// so an intermediate continuation turn's newest user text is the
		// announcement, not the original prompt. Serve a yield; the run
		// waits for the selected terminal notice.
		case strings.HasPrefix(lastUser, "batch root ") && strings.Contains(lastUser, "started; completions arrive as selected notices."):
			admissionWriteSSE(w, []string{admissionSSEText("yield", "awaiting terminal confirmation"), admissionSSEStop("yield", "stop")})

		// Turn 1: the root's own long-running command. The gated command
		// outlives the test many times over; if batch_stop resolution
		// broke, the run would genuinely wait on it.
		case strings.Contains(lastUser, "start a long job"):
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("start", "call-bash", "bash",
					`{"command":`+jsonString(command)+`,"description":"long job"}`),
				admissionSSEStop("start", "tool_calls"),
			})

		default:
			http.Error(w, "unexpected request: system="+system+" lastUser="+lastUser+" lastTool="+lastTool, http.StatusBadRequest)
		}
	})
	liveApp.Store(application)
	liveSession.Store(sessionID)
	grantRootBatchStop(t, application)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	start := time.Now()
	result, err := application.RunNonInteractiveWithResult(ctx, io.Discard, "start a long job", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.NotNil(t, result)

	// The id the model extracted from the started response is exactly the
	// id it sent to batch_stop, and the stop intent (not completion) was
	// observed before the terminal notice closed the run.
	require.NotEmpty(t, extractedRootID.Load().(string),
		"the model must have received the batch root id from the started response")
	require.True(t, stopIntentObserved.Load(),
		"batch_stop's response must have been observed as cancellation intent")
	require.True(t, terminalNoticeSeen.Load(),
		"the run must finish only after the selected terminal cancelled notice")
	require.Equal(t, "end_turn", result.ExitReason)
	require.Equal(t, "root final: batch job stopped", result.FinalText)

	// The command is gated to run indefinitely; batch_stop must terminate
	// the whole scenario far before its natural life.
	require.Less(t, elapsed, 30*time.Second,
		"batch_stop must stop the root's own command well before its natural end")
}
