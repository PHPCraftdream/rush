//go:build !windows

package tools

import (
	"errors"
	"os/exec"
	"syscall"
)

// osFailureIsFatal splits the residual OS errors of the stat/read sites in
// this package (view.go, edit.go, write.go, multiedit.go) by the
// retry-invariance criterion of the tool error contract in tools.go: fatal
// if and only if no resent input and no passage of time would make the
// call succeed. EIO, EROFS, ENOSPC and ENOMEM are failures of the storage
// medium or of the process itself — every call fails identically for the
// rest of the session — so they stay at contract level 3 and end the run.
// Everything else the OS can say about a model-chosen path — EACCES/EPERM
// (no rights on that path), ENOTDIR (a component is a file), ELOOP,
// ENAMETOOLONG, EINVAL (e.g. a NUL byte in the path), ENOENT from a file
// that vanished between stat and read, a file locked by another process —
// is correctable by the model sending a different path, so those sites
// answer with a text-error response (level 1) instead of killing the run.
//
// Unknown errnos default to recoverable on purpose: the contract requires
// proof of retry-invariance before a failure may end the run, and an
// unclassified errno proves nothing. The Windows twin of this split, over
// native Win32 codes, lives in os_failure_windows.go.
func osFailureIsFatal(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.EIO, syscall.EROFS, syscall.ENOSPC, syscall.ENOMEM:
			return true
		}
	}
	return false
}

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
