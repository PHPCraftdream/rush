// Round-4 app-layer pins (docs/reviews/2026-09-30-async-phase4-round4.md,
// W4-APP): R4C-4 (the reviewer-skip line goes to RunRequest.Stderr at the
// ExecuteRun call site) and R4C-5 (the real App's coordinator exposes every
// optional interface, R4C-5a; a CLI run on a coordinator without the reaction
// source fails loudly, R4C-5b).
package app

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// R4C-4: the "reviewer pass skipped" line of an ExecuteRun caller that does not
// run the loop (SDK) goes to RunRequest.Stderr; R3C-9 pinned reviewerPassBlocked's
// writer parameter, this pins the call site. The first turn starts an async
// bash job that never finishes, so the scope is open when the pass is due.
//
// Revert-check: passing os.Stderr at the reviewerPassBlocked call in ExecuteRun
// (app_run.go) leaves the buffer empty.
func TestExecuteRun_ReviewerPassSkipLineGoesToRequestStderr(t *testing.T) {
	gate := filepath.ToSlash(filepath.Join(t.TempDir(), "never"))
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, body []byte, _ bool, _ int) {
		_, _, lastTool := lastTurnParts(body)
		if strings.Contains(lastTool, "Async bash job") {
			loopText(w, "wait", "waiting for the job", 11, 3)
			return
		}
		command := `until [ -f "` + gate + `" ]; do :; done`
		admissionWriteSSE(w, []string{
			admissionSSEToolCall("b", "call-bash", "bash", `{"command":`+jsonString(command)+`,"description":"wait"}`),
			admissionSSEStop("b", "tool_calls"),
		})
	})
	h.app.config.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	var out, stderr syncBuffer

	res, err := h.app.ExecuteRun(loopCtx(t), RunRequest{
		Prompt:            "start the job",
		Overrides:         RunOverrides{ModelRole: config.SelectedModelTypeSmart, Origin: message.OriginCLI},
		Mode:              RunModeJSON,
		ContinueSessionID: h.sessionID,
		Origin:            message.OriginCLI,
		Stdout:            &out,
		Stderr:            &stderr,
		HideSpinner:       true,
	})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Contains(t, stderr.String(), `reviewer pass skipped for session "`+h.sessionID+`": it still has running work`)
}

// R4C-5a: the coordinator interface assertions in package agent cover the
// build; this pins the wiring the callers depend on -- a decorator wrapped
// around app.AgentCoordinator would hide the optional interfaces (every
// consumer type-asserts on the value) and silently turn the features off.
//
// Revert-check: wrapping the coordinator in a struct that embeds only
// agent.Coordinator (as a decorator would) fails all three assertions.
func TestRealApp_CoordinatorExposesEveryOptionalInterface(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})

	_, isHolder := h.app.AgentCoordinator.(agent.AutoTurnHolder)
	_, isSource := h.app.AgentCoordinator.(agent.ReactionDebtSource)
	_, isReporter := h.app.AgentCoordinator.(agent.ParkedSubAgentWorkReporter)

	require.True(t, isHolder, "AutoTurnHolder")
	require.True(t, isSource, "ReactionDebtSource")
	require.True(t, isReporter, "ParkedSubAgentWorkReporter")
}

// R4C-5b: a CLI run on a coordinator that is not a ReactionDebtSource used to
// bypass the whole loop and its driver marker without a word. The loop cannot
// run without it, so the run now fails before touching the session.
//
// Revert-check: restoring the silent fallback to a plain ExecuteRun runs the
// turn (the provider is called, no error).
func TestRunNonInteractive_CLIRunOnCoordinatorWithoutReactionSourceFails(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	real := h.app.AgentCoordinator
	h.app.AgentCoordinator = coordinatorWithoutOptionalInterfaces{Coordinator: real}
	t.Cleanup(func() { h.app.AgentCoordinator = real }) // Shutdown drives the real one

	_, out, err := h.run(loopCtx(t), RunOverrides{})

	require.Error(t, err)
	require.Contains(t, err.Error(), "ReactionDebtSource")
	require.Empty(t, out)
	require.Zero(t, h.requests.Load(), "nothing reached the provider")
}

// coordinatorWithoutOptionalInterfaces is a decorator that forwards only the
// core interface, so none of the optional ones can be asserted on it.
type coordinatorWithoutOptionalInterfaces struct{ agent.Coordinator }
