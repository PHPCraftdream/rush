//go:build windows

package tools

import (
	"os/exec"

	"github.com/PHPCraftdream/rush/internal/session"
)

// configureRunCommandProcess makes ctx cancellation (job_kill and the
// terminate_and_wake timeout, task #1023 §3) tree-kill a run_command child
// on Windows via taskkill /F /T instead of just the direct process. Mirrors
// internal/agent/tools/mcp/process_other.go's configureStdioProcess (its
// Windows build) -- same orphan-leak shape (a build tool or installer
// spawning a detached helper), same fix.
func configureRunCommandProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return session.KillProcess(cmd.Process.Pid)
	}
}
