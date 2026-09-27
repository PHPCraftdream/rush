package app

// Regression for task #1053: in a CLI run the root must be able to stop its
// OWN long-running async bash command using the job id it actually received
// ("Async bash job <id> started") -- not the internal shell id, which stays
// inside the executor goroutine until the job is already finished. Before
// the fix, job_kill only accepted shell_id, so the model had no way to
// cancel a still-running command it started itself.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// jobStartedIDPattern extracts the job id from asyncTool's "started" text
// (asyncTool.startedResponse), exactly what the model itself sees and must
// pass back to job_kill/job_output as job_id.
var jobStartedIDPattern = regexp.MustCompile(`Async bash job (\S+) started`)

func TestRunNonInteractiveRootKillsOwnAsyncJobByJobID(t *testing.T) {
	var extractedJobID atomic.Value
	extractedJobID.Store("")

	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		system, lastUser, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(system, "short title"):
			admissionWriteSSE(w, []string{admissionSSEText("title", "Job Kill By ID"), admissionSSEStop("title", "stop")})

		// Root's second ExecuteRun call (outer async-drain loop): the
		// completion notice for the killed job is fed back as a NEW user
		// message. Checked before the (stale, still-present) lastTool
		// conditions below, since this session's lastTool keeps carrying
		// job_kill's own result until a new tool call happens.
		case strings.Contains(lastUser, "Async job call-bash (bash)"):
			admissionWriteSSE(w, []string{admissionSSEText("final", "root final: job stopped"), admissionSSEStop("final", "stop")})

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

		// Turn 3: job_kill's own immediate response. Yield -- the async
		// completion for the now-terminated job arrives as the NEXT root
		// turn (matched above), not within this ExecuteRun call.
		case strings.Contains(lastTool, "terminated successfully"):
			admissionWriteSSE(w, []string{admissionSSEText("yield", "root yielded after kill"), admissionSSEStop("yield", "stop")})

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

	// Killing via job_kill must produce EXACTLY ONE completion notice for
	// the job -- not zero (the model would never learn it stopped) and not
	// two (a duplicate from both the kill and the shell's own natural exit
	// racing). Checked directly against persisted messages rather than
	// inferred from the stub's control flow, which would keep passing even
	// on a duplicate (the second notice matches the same "final" route and
	// just overwrites result.FinalText with an identical value).
	msgs, listErr := application.Messages.List(t.Context(), sessionID)
	require.NoError(t, listErr)
	var notices int
	for _, item := range msgs {
		if item.BackgroundJobNotice {
			notices++
		}
	}
	require.Equal(t, 1, notices, "exactly one completion notice may reach the session for the killed job")
}
