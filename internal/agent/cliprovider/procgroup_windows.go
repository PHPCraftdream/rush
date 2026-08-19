//go:build windows

package cliprovider

import (
	"log/slog"
	"os"
	"os/exec"

	"github.com/charmbracelet/crush/internal/session"
)

// trackChildTree registers the freshly started CLI child's whole tree
// for teardown: session.TrackProcessTree assigns it a kill-on-close Job
// Object that session.KillProcess terminates, reaching grandchildren
// the PPID walk behind taskkill /T cannot see (MSYS2/Git-Bash
// intermediary parents). Returns the pid for the matching
// UntrackProcessTree call.
//
// A failure to track (pre-Win8 nested-job limits, exotic job silos) is
// logged and otherwise ignored: the taskkill fallback inside
// KillProcess still applies, which is exactly the pre-Job-Object
// behavior.
//
// dataDir/sessionID are accepted (and ignored) only so this function's
// signature matches the Unix build's -- see procgroup_unix.go, where they
// are used to register the child's process group in the cross-process,
// generation-checked registry sessions kill reads on Unix. Windows has no
// equivalent need: KillProcess already reaches the whole tracked tree via
// the Job Object above, directly by pid, with no separate handoff.
func trackChildTree(proc *os.Process, _, _ string) int {
	if proc == nil {
		return 0
	}
	if err := session.TrackProcessTree(proc.Pid); err != nil {
		slog.Warn("cliprovider: could not track child process tree; falling back to taskkill teardown", "pid", proc.Pid, "err", err)
	}
	return proc.Pid
}

// configureChildProcessGroup is a no-op on Windows: there are no POSIX
// process groups, and tree teardown is the Job Object assigned by
// trackChildTree instead.
func configureChildProcessGroup(*exec.Cmd) {}
