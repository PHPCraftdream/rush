//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// configureRunCommandProcess makes a run_command child a process-group
// leader and replaces os/exec's default ctx-cancellation kill (which only
// signals the direct child) with a whole-group SIGKILL, so job_kill and the
// terminate_and_wake timeout (task #1023 §3) take down every descendant the
// program spawns, not just itself. Mirrors
// internal/agent/tools/mcp/process_unix.go's configureStdioProcess -- same
// orphan-leak shape, same fix.
func configureRunCommandProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
