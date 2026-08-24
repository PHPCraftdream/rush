package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"runtime"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/agentguard"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

// encodePowerShellPayload builds the base64(UTF-16LE) blob that
// `powershell -EncodedCommand` expects, mirroring agentguard's decoder.
func encodePowerShellPayload(t *testing.T, src string) string {
	t.Helper()
	u16 := make([]byte, 0, len(src)*2)
	for _, r := range src {
		u16 = append(u16, byte(r), 0)
	}
	return base64.StdEncoding.EncodeToString(u16)
}

// shellIDSet snapshots the background-shell manager's visible shell IDs.
// NewBashTool runs every command through BackgroundShellManager.Start
// (internal/shell/background.go), which registers the shell in its map
// BEFORE spawning the process — so a set that did not grow across a tool
// call proves the command never reached execution. That is the property
// the refusal tests below need: a check that fired "too late" would have
// let Start run, and the window would already be open.
func shellIDSet() map[string]bool {
	out := map[string]bool{}
	for _, id := range shell.GetBackgroundShellManager().List() {
		out[id] = true
	}
	return out
}

// TestBashTool_RefusesWindowOpenersBeforeExecution is the regression test
// for the built-in Bash tool having wired only agentguard.Check while the
// cliprovider MCP Bash tool ran both guards: `start`-family verbs slipped
// through this surface and popped real, visible windows on the operator's
// desktop despite the outer process's HideWindow attribute.
//
// Each case asserts BOTH that the refusal carries the WindowOpenerError
// message AND that no background shell was started — the second assertion
// is what pins the check to BEFORE any shell runs; matching the error
// text alone would pass even if the check fired after the process
// launched.
//
// NOT t.Parallel: it observes the process-wide background shell manager
// singleton, which sequential tests share (same reason as
// TestBashTool_BackgroundShellStartLimitIsModelVisibleNotFatal).
func TestBashTool_RefusesWindowOpenersBeforeExecution(t *testing.T) {
	if runtime.GOOS != "windows" {
		// The window-opener guard is Windows-only by design (the verbs are
		// cmd.exe/PowerShell constructs) — CI's ubuntu/macos legs must not
		// fail on it. The test runs fully on the windows leg.
		t.Skip("window-opener guard is Windows-only (runtime.GOOS gate inside agentguard.CheckAll)")
	}

	cases := []struct {
		name    string
		command string
	}{
		{"direct start", "start notepad"},
		{"cmd /c wrapper", `cmd /c start notepad`},
		{"powershell -Command", `powershell -Command "Start-Process notepad"`},
		{"encoded command", "powershell -EncodedCommand " + encodePowerShellPayload(t, "Start-Process notepad")},
		{"env wrapper", "env start notepad"},
		{"timeout wrapper", "timeout 5 start notepad"},
	}

	tool := newBashToolForTest(t.TempDir())
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "window-safety-session")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := shellIDSet()

			resp := runBashTool(t, tool, ctx, BashParams{
				Description: tc.name,
				Command:     tc.command,
			})

			// The refusal must surface the WindowOpenerError itself. The
			// expected message is computed from the detector rather than
			// hardcoded, so wording changes stay in sync while the tool's
			// surfacing of THIS error stays pinned.
			winErr := agentguard.CheckWindowSafety(tc.command)
			require.NotNil(t, winErr, "test bug: detector no longer flags %q", tc.command)
			require.True(t, resp.IsError, "tool must refuse: %s", tc.command)
			require.Contains(t, resp.Content, winErr.Error())

			// And it must have refused BEFORE starting any shell.
			require.Equal(t, before, shellIDSet(),
				"the refusal must happen before BackgroundShellManager.Start — "+
					"a window would already be open otherwise: %s", tc.command)
		})
	}
}

// TestBashTool_WindowSafetyControlCaseStillExecutes is the control for the
// refusal test above, through the same shell-manager snapshot: an ordinary
// command with no window-opener verb must still reach
// BackgroundShellManager.Start and succeed — proving the spy can observe
// execution (a spy that never fired would prove nothing) and that the new
// guard does not over-block ordinary commands.
//
// It uses the auto-background path deliberately: the synchronous path
// removes the shell from the manager before tool.Run returns, so a
// still-listed shell can only be observed on a command that is handed
// back as a background job.
func TestBashTool_WindowSafetyControlCaseStillExecutes(t *testing.T) {
	tool := newBashToolForTest(t.TempDir())
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "window-safety-control-session")

	before := shellIDSet()
	resp := runBashTool(t, tool, ctx, BashParams{
		Description:         "window-safety control case",
		Command:             "sleep 5 && echo window-safety-control-ok",
		AutoBackgroundAfter: 1,
	})

	require.False(t, resp.IsError, "ordinary command must not be refused: %s", resp.Content)

	var meta BashResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.True(t, meta.Background, "control command should have been auto-backgrounded")
	require.NotEmpty(t, meta.ShellID)
	require.False(t, before[meta.ShellID],
		"the control command must run through a NEW background shell")

	t.Cleanup(func() {
		_ = shell.GetBackgroundShellManager().Kill(context.Background(), meta.ShellID)
	})
}
