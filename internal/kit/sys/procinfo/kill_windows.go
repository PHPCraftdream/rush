//go:build windows

package procinfo

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	"golang.org/x/sys/windows"
)

// KillProcess forcibly terminates the process tree rooted at pid. It is
// KillProcessWithPass without a tracked-job pass; callers that registered
// pids with proctrack.TrackProcessTree kill through proctrack.KillProcess,
// which supplies the pass.
func KillProcess(pid int) error { return KillProcessWithPass(pid, nil) }

// KillProcessWithPass forcibly terminates the process tree rooted at pid,
// running pass (when non-nil) as the Job Object pass described below.
// pass terminates the tracked Job Object for pid, if any, and reports
// whether it did; procinfo cannot import the registry that owns it
// (internal/kit/sys/proctrack), so the pass is injected per call instead
// of through a process-global hook.
//
// Tracked pids (registered via TrackProcessTree at spawn time) are killed
// by BOTH mechanisms below, deliberately not short-circuited: taskkill
// /F /T first, then the Job Object. The two mechanisms have disjoint
// blind spots — taskkill /T walks the PPID chain and so cannot see past
// the MSYS2/Git-Bash transient intermediary parent (job membership
// reaches those), while the job cannot see a process that escaped in the
// Start→AssignProcessToJobObject micro-gap and IS PPID-reachable when
// rush itself does not run under MSYS (taskkill reaches those).
// Returning early on a successful TerminateJobObject — the shape this
// function had after 68f9c65f — silently dropped that second case: the
// job terminated "successfully", KillProcess reported nil, and the
// escapee survived with its fallback explicitly disabled. taskkill runs
// BEFORE the job pass because /T needs the root alive to walk from and
// the job termination kills the root. Cost: one taskkill spawn per kill
// of a tracked child; kill paths only run on stream cancellation, never
// per-stream, so this is not hot.
//
// On Windows os.Process.Kill() goes through OpenProcess(PROCESS_TERMINATE)
// which can fail with "Access is denied" for processes spawned under a
// different shell (Git Bash / MSYS, elevated console host, etc.). We
// prefer two more reliable paths:
//
//  1. taskkill /F /T /PID <pid> — kills the process plus every child it
//     spawned. This is what rush sessions kill actually wants because
//     a stuck rush.exe usually has a claude.cmd / node.exe descendant
//     still holding its stdin pipe.
//  2. As a fallback (taskkill not on PATH) OpenProcess + TerminateProcess
//     via golang.org/x/sys/windows.
//
// Returns nil if the process is already gone. Caller is expected to poll
// IsProcessAlive afterwards.
func KillProcessWithPass(pid int, pass func(pid int) bool) error {
	if pid <= 0 {
		return fmt.Errorf("KillProcess: invalid pid %d", pid)
	}
	if !isProcessAlive(pid) {
		// Already gone. Still consume any tracked entry so it cannot
		// later collide with a recycled pid (see proctrack's terminateTrackedJob);
		// closing its KILL_ON_JOB_CLOSE handle also tears down any
		// straggler descendant still inside the job.
		if pass != nil {
			pass(pid)
		}
		return nil
	}
	if path, lookErr := exec.LookPath("taskkill"); lookErr == nil {
		cmd := platform.Command(context.Background(), path, "/F", "/T", "/PID", fmt.Sprintf("%d", pid))
		_, _ = cmd.CombinedOutput()
		// taskkill errors are not surfaced directly: the pid-based
		// aliveness checks below decide the outcome, which also covers
		// taskkill /T missing an MSYS-broken chain it cannot walk.
	}
	// Job pass: one TerminateJobObject takes down the tracked tree,
	// MSYS-broken chains included. Identity-checked inside, so a stale
	// entry whose pid the OS has since recycled is closed without
	// faking success.
	if pass != nil {
		pass(pid)
	}
	if !isProcessAlive(pid) {
		return nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		if !isProcessAlive(pid) {
			return nil
		}
		return fmt.Errorf("KillProcess: OpenProcess %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		if !isProcessAlive(pid) {
			return nil
		}
		return fmt.Errorf("KillProcess: TerminateProcess %d: %w", pid, err)
	}
	return nil
}

// IsProcessAlive is the exported variant of isProcessAlive, used by
// `rush sessions reap` to probe lock holders from the CLI layer.
func IsProcessAlive(pid int) bool { return isProcessAlive(pid) }

func isProcessAlive(pid int) bool {
	return isProcessAliveWithOpenProcess(pid, windows.OpenProcess)
}

// isProcessAliveWithOpenProcess reports whether a process with the given PID is currently
// running. Used to detect orphan locks where the holder crashed without
// releasing — on Windows we open the process with PROCESS_QUERY_LIMITED_
// INFORMATION (cheap, doesn't require admin), then ask for its exit code.
// STILL_ACTIVE (259) means alive; anything else means it has exited.
func isProcessAliveWithOpenProcess(pid int, openProcess func(uint32, bool, uint32) (windows.Handle, error)) bool {
	if pid <= 0 {
		return false
	}
	h, err := openProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
