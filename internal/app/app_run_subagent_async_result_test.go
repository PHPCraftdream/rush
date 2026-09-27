package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// Regression: in a CLI run a delegated sub-agent's own async bash result
// must reach that sub-agent as a new turn, as the async tool promises
// ("its result will arrive as a new session message"). The child's CLI
// completion used to land on the child session's ready queue, which only
// the ROOT loop drains, so the child never saw its command's output and the
// parent was told the delegation finished with the child's pre-result text.
func TestRunNonInteractiveChildReceivesItsOwnAsyncBashResult(t *testing.T) {
	const marker = "BUILD-MARKER-42"
	var childSawResult atomic.Bool
	var rootFinal atomic.Value
	rootFinal.Store("")

	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		system, lastUser, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(system, "short title"):
			admissionWriteSSE(w, []string{admissionSSEText("title", "Child Result"), admissionSSEStop("title", "stop")})
		case strings.Contains(lastUser, "Async job call-agent (agent)"):
			rootFinal.Store(lastUser)
			admissionWriteSSE(w, []string{admissionSSEText("root-final", "root done"), admissionSSEStop("root-final", "stop")})
		case strings.Contains(lastUser, "Async job call-bash (bash)"):
			childSawResult.Store(strings.Contains(lastUser, marker))
			admissionWriteSSE(w, []string{admissionSSEText("child-result", "child done: build output received"), admissionSSEStop("child-result", "stop")})
		case strings.Contains(lastTool, "Async bash job"):
			admissionWriteSSE(w, []string{admissionSSEText("child-wait", "child waiting for build"), admissionSSEStop("child-wait", "stop")})
		case strings.Contains(lastUser, "WORKER-MARKER"):
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("child-bash", "call-bash", "bash",
					`{"command":`+jsonString(shortBuildCommand(marker))+`,"description":"build"}`),
				admissionSSEStop("child-bash", "tool_calls"),
			})
		case strings.Contains(lastTool, "Async agent job call-agent started"):
			admissionWriteSSE(w, []string{admissionSSEText("root-yield", "root waiting"), admissionSSEStop("root-yield", "stop")})
		default:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("delegate", "call-agent", "agent", `{"prompt":"WORKER-MARKER build it"}`),
				admissionSSEStop("delegate", "tool_calls"),
			})
		}
	})
	grantChildBackgroundShellTools(t, application)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	result, err := application.RunNonInteractiveWithResult(ctx, io.Discard, "delegate a build", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.True(t, childSawResult.Load(),
		"the sub-agent never received its own bash result; parent was told: %q", rootFinal.Load())
	require.Contains(t, rootFinal.Load().(string), "child done",
		"the parent must receive the child's post-result answer, not its pre-result text")
}

// lastTurnParts returns the system prompt and the newest user and tool
// message texts of a provider request.
func lastTurnParts(body []byte) (system, lastUser, lastTool string) {
	var req wireRequest
	if json.Unmarshal(body, &req) != nil {
		return "", "", ""
	}
	for i, m := range req.Messages {
		s, _ := m.Content.(string)
		switch {
		case i == 0 && m.Role == "system":
			system = s
		case m.Role == "user":
			lastUser = s
		case m.Role == "tool":
			lastTool = s
		}
	}
	return system, lastUser, lastTool
}

// shortBuildCommand outlives bash's 1s fast-failure probe, then prints marker.
func shortBuildCommand(marker string) string {
	if runtime.GOOS == "windows" {
		return "ping -n 4 127.0.0.1 && echo " + marker
	}
	return "sleep 3 && echo " + marker
}
