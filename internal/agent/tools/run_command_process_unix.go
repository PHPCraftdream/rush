//go:build !windows

package tools

import (
	"errors"
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
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		// ESRCH: the group is gone. EPERM: macOS refuses to signal a group
		// whose members have all exited but are not reaped yet (we own the
		// group, so it cannot be a real permission failure).
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
			return nil
		}
		return err
	}
}
