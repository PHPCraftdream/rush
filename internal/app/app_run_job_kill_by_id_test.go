package app

// Regression for task #1053: in a CLI run the root must be able to stop its
// OWN long-running async bash command using the job id it actually received
// ("Async bash job <id> started") -- not the internal shell id, which stays
// inside the executor goroutine until the job is already finished. Before
// the fix, job_kill only accepted shell_id, so the model had no way to
// cancel a still-running command it started itself.
//
// Updated for task #1063 (phase-4 durable-core step 6): job_kill's own tool
// result now carries the real output snapshot directly (delivery='done' the
// instant the transition commits, doc sec.3.2/3.4) instead of a generic
// "terminated successfully" placeholder followed by a SEPARATE completion
// notice on a later root turn. This test's fixture and assertions moved
// accordingly: the "final" turn is job_kill's own turn now, not a second
// ExecuteRun round-trip, and exactly ZERO BackgroundJobNotice messages are
// expected for the killed job (its row is never pulled as a notice at all).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// jobStartedIDPattern extracts the job id from asyncTool's "started" text
// (asyncTool.startedResponse), exactly what the model itself sees and must
// pass back to job_kill/job_output as job_id.
var jobStartedIDPattern = regexp.MustCompile(`Async bash job (\S+) started`)

func TestRunNonInteractiveRootKillsOwnAsyncJobByJobID(t *testing.T) {
	if f6BatchOn() {
		testRunNonInteractiveRootKillsOwnBatchJobByStop(t)
		return
	}
	var extractedJobID atomic.Value
	extractedJobID.Store("")

	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		system, lastUser, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(system, "short title"):
			admissionWriteSSE(w, []string{admissionSSEText("title", "Job Kill By ID"), admissionSSEStop("title", "stop")})

		// Turn 2: the bash tool's "started" result is the newest tool
		// result. Extract the job id from it exactly as the model would,
		// then call job_kill with job_id (not shell_id).
		case jobStartedIDPattern.MatchString(lastTool):
			m := jobStartedIDPattern.FindStringSubmatch(lastTool)
			if len(m) != 2 {
				http.Error(w, "could not extract job id from: "+lastTool, http.StatusBadRequest)
				return
			}
			extractedJobID.Store(m[1])
			// bash.go's run_in_background path sleeps 1s (fast-failure
			// probe) before returning the shell id into the response
			// metadata that awaitShell reads to record it in the work
			// ledger. Wait comfortably past that so job_kill's job_id
			// resolution never races "job is still starting".
			time.Sleep(3 * time.Second)
			input, marshalErr := json.Marshal(map[string]string{"job_id": m[1]})
			if marshalErr != nil {
				http.Error(w, marshalErr.Error(), http.StatusInternalServerError)
				return
			}
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("kill", "call-kill", "job_kill", string(input)),
				admissionSSEStop("kill", "tool_calls"),
			})

		// Turn 3: job_kill's own immediate response now carries the real
		// output snapshot directly (task #1063) -- the run ends HERE, no
		// second notice/turn follows.
		case strings.Contains(lastTool, "was stopped (job_kill)"):
			admissionWriteSSE(w, []string{admissionSSEText("final", "root final: job stopped"), admissionSSEStop("final", "stop")})

		// Turn 1: the initial prompt. Start a command that outlives the
		// test many times over so a broken job_id resolution would leave
		// the run genuinely waiting on it, not racing to a coincidental
		// finish.
		case strings.Contains(lastUser, "start a long job"):
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("start", "call-bash", "bash",
					`{"command":`+jsonString(longRunningCommand())+`,"description":"long job"}`),
				admissionSSEStop("start", "tool_calls"),
			})

		default:
			http.Error(w, "unexpected request: system="+system+" lastUser="+lastUser+" lastTool="+lastTool, http.StatusBadRequest)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	start := time.Now()
	result, err := application.RunNonInteractiveWithResult(ctx, io.Discard, "start a long job", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, "call-bash", extractedJobID.Load().(string),
		"the model must have received and used the job id from the started notice")
	require.Equal(t, "end_turn", result.ExitReason)
	require.Equal(t, "root final: job stopped", result.FinalText)

	// The command itself runs for ~60s; job_kill by job_id must stop it
	// long before that natural end.
	require.Less(t, elapsed, 30*time.Second,
		"job_kill(job_id) must stop the command well before its natural end")

	// Task #1063: killing via job_kill must produce ZERO background-job
	// notices for the job -- its result is job_kill's OWN tool answer, and
	// the row commits delivery='done' the instant the transition does, so it
	// is never a pull candidate (no second notice, and so no double
	// completion either, from both the kill and the shell's own natural
	// exit racing). Checked directly against persisted messages rather than
	// inferred from the stub's control flow.
	msgs, listErr := application.Messages.List(t.Context(), sessionID)
	require.NoError(t, listErr)
	var notices int
	for _, item := range msgs {
		if item.BackgroundJobNotice {
			notices++
		}
	}
	require.Equal(t, 0, notices, "job_kill's result is its own tool answer, never a separate background-job notice")
}

// grantRootBatchStop adds batch_stop to the ROOT (coder) agent's allowed
// tools. batch_allowed_controls.go grants batch_stop BY NAME from the
// allowed list (never via job_kill), so without this the root's batch_stop
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

// testRunNonInteractiveRootKillsOwnBatchJobByStop is the flag-ON twin: the
// ROOT's OWN bash call escapes the inline window and returns the batch
// started text ("batch root <id> started; completions arrive as selected
// notices."). The root extracts the advertised root id and issues batch_stop
// -- whose result is cancellation INTENT (accepted, not terminal) -- and the
// run may only finish after the selected terminal notice confirms state
// "cancelled".
func testRunNonInteractiveRootKillsOwnBatchJobByStop(t *testing.T) {
	command, release := recoveryJobCommand(t)
	defer releaseRecoveryJob(t, release)
	var extractedRootID atomic.Value
	extractedRootID.Store("")
	var stopIntentObserved atomic.Bool
	var terminalNoticeSeen atomic.Bool
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		system, lastUser, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(system, "short title"):
			admissionWriteSSE(w, []string{admissionSSEText("title", "Job Kill By ID"), admissionSSEStop("title", "stop")})

		// The selected terminal notice: `batch <root> node <node>: kind
		// "task" state "cancelled" ...` arrived as the root's next user
		// turn. This is the CONFIRMED terminal event; only now may the run
		// finish. (Checking the DB row as well makes the ordering
		// production-proof, not stub-proof.)
		case strings.HasPrefix(lastUser, "batch "+extractedRootID.Load().(string)+" node ") && strings.Contains(lastUser, `state "cancelled"`):
			var state string
			err := application.DB().QueryRowContext(r.Context(), `SELECT state FROM batch_nodes WHERE owner_session_id = ? AND root_id = ? AND task_kind = 'shell'`, sessionID, extractedRootID.Load().(string)).Scan(&state)
			if err != nil || state != "cancelled" {
				http.Error(w, "terminal notice before the command row was cancelled: "+state, http.StatusServiceUnavailable)
				return
			}
			terminalNoticeSeen.Store(true)
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
