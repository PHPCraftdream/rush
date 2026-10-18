//go:build !windows

package cliprovider

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/PHPCraftdream/rush/internal/session"
)

// trackChildTree is the Unix counterpart of the Windows Job Object
// registration in procgroup_windows.go. The ORDINARY teardown paths need
// nothing from it — killChildProcess group-kills the child directly via
// its own syscall.Kill(-pid, SIGKILL) — but registering the child lets
// the last-resort sweep in session.KillAllTrackedTrees find it when rush
// is about to die without running its own cleanup (the second Ctrl-C;
// see internal/cmd/run.go). Windows gets that case for free from
// KILL_ON_JOB_CLOSE; Unix does not.
//
// Both spawn paths make the child its own process-group leader BY
// CONSTRUCTION — the pipe branch via configureChildProcessGroup's
// Setpgid, the PTY branch via go-pty's cmd_unix.go start() which
// force-sets SysProcAttr{Setsid: true, Setctty: true} — so proc.Pid IS
// the registered group's id, and no liveness probe is needed to prove
// leadership.
//
// The registration gate below is group EXISTENCE (kill(-pgid, 0)), not
// leader liveness: trackChildTree runs right after Start, and a leader
// that exits before we get here (macOS getpgid reports ESRCH even for a
// zombie; a reaped leader has no /proc entry anywhere) must not prevent
// registering a group whose grandchildren still live — the same
// mechanism the #65 review filed as P3-5 and P2-1.
//
// session.TrackProcessTree above still self-filters non-leaders by its
// own contract: a reaped leader is simply not recorded in the in-memory
// sweep map — the DURABLE registry below is what "sessions kill" reads
// from another process.
//
// It ALSO durably records the group (session.RegisterChildGroup) in a
// cross-process, on-disk registry keyed by this rush process's own pid.
// TrackProcessTree/KillAllTrackedTrees above only help when THIS process
// is still alive to run its own signal handler; `rush sessions kill`
// runs as a separate OS process and has no access to the in-memory
// trackedGroups map, so without the disk registry it could kill the
// rush pid it read from the lock file and still leave this exact
// process group (a CLI provider's claude/gemini/codex/qwen tree) running
// — the #580 gap. See childgroup_registry_unix.go for the full design
// rationale.
func trackChildTree(proc *os.Process, dataDir, sessionID string) int {
	if proc == nil {
		return 0
	}
	_ = session.TrackProcessTree(proc.Pid)
	if processGroupExists(proc.Pid) && dataDir != "" && sessionID != "" {
		// generation ties this registration to the CURRENT holder of
		// sessionID's lock: read fresh from disk right here (this
		// process IS that holder, having just acquired the lock before
		// Stream ever ran, so the sidecar is expected to be present and
		// current -- a miss just means the registration is silently
		// skipped, which is the correct fail-closed behavior for a
		// best-effort augmentation). See
		// internal/session/childgroup_registry_unix.go for the full
		// generation-checked design and why a bare pid is not enough.
		lockPath := session.SessionLockPath(dataDir, sessionID)
		if generation := session.ReadLockGeneration(lockPath); generation != "" {
			session.RegisterChildGroup(dataDir, sessionID, proc.Pid, generation)
		}
	}
	return proc.Pid
}

// processGroupExists reports whether the process group named pgid still
// has ANY living member (kill(-pgid, 0) is an existence/permission check
// that sends no signal), as opposed to the leader itself being alive: a
// group survives while any forked-in grandchild lives, even after the
// leader has exited and been reaped -- which is exactly the state
// isProcessGroupLeader's Getpgid probe could not see (macOS getpgid
// reports ESRCH for an exited leader, and a reaped leader has no /proc
// entry anywhere). EPERM also means the group exists (it holds a process
// we may not signal).
func processGroupExists(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || err == syscall.EPERM
}

// configureChildProcessGroup makes a pipe-branch CLI child a
// process-group leader (pgid == pid) and installs cancellation to kill
// its whole tree — the direct child plus every grandchild it spawns
// (claude.cmd → node.exe chains, MCP servers the CLI itself starts, …)
// — with one killpg. This mirrors internal/agent/tools/mcp/
// process_unix.go, where the same orphan-leak shape on stdio MCP
// servers was fixed the same way.
//
// The PTY branch is configured separately by go-pty, which establishes
// its own session/process-group behavior and retains its separate teardown
// path; this helper is specifically for children created through pipes.
func configureChildProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		return killChildProcess(cmd.Process)
	}
}

// killChildProcess targets the group established by configureChildProcessGroup,
// even if the session leader has already exited and can no longer be inspected.
func killChildProcess(proc *os.Process) error {
	if proc == nil {
		return os.ErrProcessDone
	}
	return killChildProcessPid(proc.Pid)
}

// killChildProcessPid is the pid-taking form of killChildProcess, for
// callers that must not dereference an *os.Process at all -- the PTY
// Cancel closure reads the *os.Process published by provider_stream.go's
// ptyProc atomic instead of dereferencing go-pty's Cmd.Process (assigned
// only after the inner Start returns, racing the ctx watcher that invokes
// Cancel) and dispatches through killChildProcess.
//
// ESRCH maps to os.ErrProcessDone, not nil: when this runs as an
// exec.Cmd.Cancel, a nil return makes Wait report the context's error for
// a command that had already exited successfully and been reaped, where
// the default Process.Kill's own os.ErrProcessDone let Wait keep the
// command's usual exit status (toolchain doc on Cmd.Cancel). Non-Cancel
// callers discard the error either way.
func killChildProcessPid(pid int) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-pid, syscall.SIGKILL)
	switch err {
	case nil:
		return nil
	case syscall.ESRCH:
		return os.ErrProcessDone
	default:
		return err
	}
}
