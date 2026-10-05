// End-to-end through the real `rush run` loop (#1177): the root delegates an
// `agent` job, the worker schedules its own wake timer, and on the woken turn
// calls ask_question. The question must reach the parent as a turn input
// (the #1157 release-carried child_question path), the parent must answer
// with agent(resume_session_id=...), the worker must report back, and the run
// must end end_turn -- never awaiting_answer, and never with the child's turn
// error surfacing as the run's error.
//
// Dispatch keys on WHO asks (system prompt) and a per-role scripted turn
// counter, not on history substrings: whole-body matching is what misrouted
// earlier attempts (see app_run_subagent_root_wait_test.go's harness notes).
// The child session id for the resume call is parsed from the question text
// itself, the only place the dynamic id exists.
//
// Why the HELD-question shape (worker asks while its own jobs run) is NOT
// scripted here: on current main the own-jobs reader (app_run_setup.go) is
// wired into the worker's wake-driven turns too, so ask_question with own
// running work hands back the keep-alive hint instead of force-finishing
// (342af913); a question-stop with open own scope is unreachable through the
// model surface in this process, and the held path is covered at the
// coordinator level (internal/agent delegation_question_test.go T1/T2).
//
// Revert-check (TestRunNonInteractive_ChildQuestionRoundTrip): disabling
// refreshSubAgentCompletion's question branch (coordinator_work_scope.go,
// the subAgentQuestionFromFinish read) drops the question from the release;
// the parent never issues the resume call and the run fails -- red (verified
// with the mutant; reverted).
package app

import (
	"context"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

var childQuestionSessionRe = regexp.MustCompile(`SUB-AGENT QUESTION \(session ([^)]+)\)`)

// grantChildWakeTools layers wakein and ask_question onto the delegated
// sub-agent's toolset, mirroring grantChildBackgroundShellTools: the
// read-only task toolset excludes ask_question, and without the grant the
// worker's ask call is a "tool not found" step error, not a question.
func grantChildWakeTools(t *testing.T, application *App) {
	t.Helper()
	cfg := application.config.Config()
	task, ok := cfg.Agents[config.AgentTask]
	require.True(t, ok, "the delegated task agent must be configured")
	tools := slices.Clone(task.AllowedTools)
	for _, name := range []string{"wakein", "ask_question"} {
		if !slices.Contains(tools, name) {
			tools = append(tools, name)
		}
	}
	application.config.UpdateAgentAllowedTools(config.AgentTask, tools)
}

func TestRunNonInteractive_ChildQuestionRoundTrip(t *testing.T) {
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 50*time.Millisecond, 0)()

	var rootTurns, childTurns atomic.Int32
	var questionReachedParent atomic.Bool
	var childSessionID atomic.Value // string
	var output syncBuffer

	var application *App
	var sessionID string
	application, sessionID = newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		system, lastUser, _ := lastTurnParts(body)
		role := "root"
		if strings.Contains(system, "an agent for Rush") {
			role = "child"
		}
		switch {
		case strings.Contains(lastUser, "Generate a concise title"):
			admissionWriteSSE(w, []string{admissionSSEText("title", "Child Question E2E"), admissionSSEStop("title", "stop")})
		case role == "child":
			// The worker (task agent). Scripted by turn counter.
			switch childTurns.Add(1) {
			case 1:
				// The delegation turn itself asks the question. No own async
				// work runs, so the question legitimately force-finishes the
				// turn, the delegation release carries it to the parent, and
				// the session stays resumable for the answer.
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("child-ask", "call-ask", "ask_question",
						`{"question":"which port should the build use?","options":["8080","9090"]}`),
					admissionSSEStop("child-ask", "tool_calls"),
				})
			case 2:
				// The answer turn (the parent resumed the session): report
				// and finish so the resumed delegation delivers its result.
				admissionWriteSSE(w, []string{
					admissionSSEText("child-final", "child final: build used the answered port"),
					admissionSSEStop("child-final", "stop"),
				})
			default:
				admissionWriteSSE(w, []string{
					admissionSSEText("child-extra", "child idle"),
					admissionSSEStop("child-extra", "stop"),
				})
			}
		default:
			// The root (coder agent). The question can land in TWO places, and
			// which one is a race against the root's own step timing: the
			// delegation turn asks at once, so on a slow machine the release
			// carrying the question is pulled into the root's NEXT provider
			// step -- the "yield" step -- instead of arriving as a Drain turn
			// after it. The script therefore keys the answer on the question
			// text in the prompt, not on a turn number, and answers it
			// wherever it first appears.
			turn := rootTurns.Add(1)
			if m := childQuestionSessionRe.FindStringSubmatch(lastUser); m != nil && questionReachedParent.CompareAndSwap(false, true) {
				childSessionID.Store(m[1])
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("root-answer", "call-answer", "agent",
						`{"resume_session_id":`+jsonString(m[1])+`,"prompt":"use port 9090 and finish"}`),
					admissionSSEStop("root-answer", "tool_calls"),
				})
				return
			}
			switch {
			case turn == 1:
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("delegate", "call-agent", "agent", `{"prompt":"WORKER-Q do the task"}`),
					admissionSSEStop("delegate", "tool_calls"),
				})
			case !questionReachedParent.Load():
				// Nothing to answer yet: yield; the question comes later as a
				// Drain turn.
				admissionWriteSSE(w, []string{
					admissionSSEText("root-yield", "root yielded: delegation parked"),
					admissionSSEStop("root-yield", "stop"),
				})
			default:
				admissionWriteSSE(w, []string{
					admissionSSEText("root-final", "root final answer"),
					admissionSSEStop("root-final", "stop"),
				})
			}
		}
	})
	grantChildBackgroundShellTools(t, application)
	grantChildWakeTools(t, application)

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	res, err := application.RunNonInteractiveWithResult(ctx, &output, "start a worker",
		RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, res)

	require.True(t, questionReachedParent.Load(),
		"the child's question must reach the parent as a prompt turn")
	require.NotEqual(t, "awaiting_answer", res.ExitReason,
		"the child's question must pause the WORKER, not exit the whole run: %v", res.Error)
	require.NotContains(t, output.String(), "call already attempted",
		"the child's turn error must not surface as the run's error")
	require.Equal(t, "end_turn", res.ExitReason)
	require.Equal(t, "root final answer", res.FinalText)
	require.NotEmpty(t, childSessionID.Load(), "the resume used the child session id from the question text")

	// The delegation notices: the first release carries the QUESTION, the
	// resumed delegation's release carries the child's POST-answer result.
	msgs, listErr := application.Messages.List(context.Background(), sessionID)
	require.NoError(t, listErr)
	var questionNotice, resultNotice int
	for _, item := range msgs {
		if !item.BackgroundJobNotice {
			continue
		}
		switch {
		case strings.Contains(item.FullText(), "SUB-AGENT QUESTION"):
			questionNotice++
			require.Contains(t, item.FullText(), "which port should the build use?",
				"the question notice must carry the question itself")
		case strings.Contains(item.FullText(), "build used the answered port"):
			resultNotice++
		}
	}
	require.Equal(t, 1, questionNotice, "exactly one question-carrying notice may reach the parent")
	require.Equal(t, 1, resultNotice, "exactly one result-carrying notice may reach the parent")
}
