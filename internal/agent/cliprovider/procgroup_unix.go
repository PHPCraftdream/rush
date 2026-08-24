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
// nothing from it — KillProcess detects process-group leaders itself via
// getpgid — but registering the child lets the last-resort sweep in
// session.KillAllTrackedTrees find it when rush is about to die without
// running its own cleanup (the second Ctrl-C; see internal/cmd/run.go).
// Windows gets that case for free from KILL_ON_JOB_CLOSE; Unix does not.
//
// session.TrackProcessTree records only pids that are their own group
// leader, so a child spawned without Setpgid is silently not registered —
// which is correct: there is no group of ours to sweep.
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
	if isProcessGroupLeader(proc.Pid) && dataDir != "" && sessionID != "" {
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

// isProcessGroupLeader reports whether pid is the leader of its own
// process group (pgid == pid). Mirrors the identical check
// session.TrackProcessTree performs internally; duplicated here (rather
// than exported from session) because RegisterChildGroup deliberately
// keeps its own contract simple ("caller must pass only confirmed
// leaders") instead of silently swallowing non-leader pids like
// TrackProcessTree does — this package is the only caller and always
// wants to know definitively before deciding whether to register at all.
func isProcessGroupLeader(pid int) bool {
	pgid, err := syscall.Getpgid(pid)
	return err == nil && pgid == pid
}

// configureChildProcessGroup makes a pipe-branch CLI child a
// process-group leader (pgid == pid) so session.KillProcess can kill
// its whole tree — the direct child plus every grandchild it spawns
// (claude.cmd → node.exe chains, MCP servers the CLI itself starts, …)
// — with one killpg. This mirrors internal/agent/tools/mcp/
// process_unix.go, where the same orphan-leak shape on stdio MCP
// servers was fixed the same way.
//
// The PTY branch must NOT grow this: go-pty's unixPty.start() already
// sets SysProcAttr{Setsid: true, Setctty: true}, and a session leader
// is by definition a process-group leader, so KillProcess's group-kill
// path applies there unconditionally.
func configureChildProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
