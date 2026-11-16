//go:build windows

package shexec

import (
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrJobUnavailable is the sentinel returned by assignProcessJob when a
// per-command Windows Job Object could not be created, configured, or the
// child could not be assigned to it. Callers must treat it as a silent
// fallback trigger: the command continues exactly as before and teardown
// relies on the taskkill /T style tree kill alone. A job failure must
// never fail the command itself.
var ErrJobUnavailable = errors.New("per-command job object unavailable")

// maxProcessesPerCommand bounds how many processes a single shell command
// may have alive inside its Job Object at once. A legitimate go build or
// make -j tree stays far below it; a runaway fork bomb or a PATH shim that
// re-executes itself forever hits the limit and the OS refuses (or the
// limit is enforced at creation), so the command collapses into an error
// instead of pinning the CPU and holding the output pipe open forever.
//
// This is a package variable, not a constant, so tests can lower it via
// SetMaxProcessesPerCommand and exercise the bound in seconds instead of
// minutes.
var maxProcessesPerCommand = 2048

// SetMaxProcessesPerCommand overrides maxProcessesPerCommand for the
// duration of the returned restore function. Intended for tests only.
func SetMaxProcessesPerCommand(n int) (restore func()) {
	prev := maxProcessesPerCommand
	maxProcessesPerCommand = n
	return func() { maxProcessesPerCommand = prev }
}

// commandJob tracks the Job Object handle for one started command. The
// handle is stored under a mutex because the kill callback can run on a
// different goroutine than the start path that created the job.
type commandJob struct {
	mu     sync.Mutex
	handle windows.Handle
}

// assign starts the given already-started command's job bookkeeping:
// a Job Object with JOB_OBJECT_LIMIT_ACTIVE_PROCESS set to the current
// maxProcessesPerCommand is created and the process is assigned to it.
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE is deliberately NOT set: a daemon a
// command leaves running behind it must survive the command's completion,
// exactly as it does today. The limit stays in force for the job's
// lifetime (until close). Any failure closes the job handle and returns
// ErrJobUnavailable so callers silently fall back to the taskkill /T
// style tree kill; the command itself never fails because of this.
func assignProcessJob(cmd *exec.Cmd, j *commandJob) error {
	if cmd == nil || cmd.Process == nil {
		slog.Debug("Per-command job object unavailable; falling back to taskkill", "step", "OpenProcess", "error", "nil process")
		return ErrJobUnavailable
	}
	pid := cmd.Process.Pid
	// os.Process does not expose its handle, so open one ourselves with
	// exactly the rights the two calls below need.
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		logJobFallback(pid, "OpenProcess", err)
		return fmt.Errorf("%w: OpenProcess: %v", ErrJobUnavailable, err)
	}
	defer windows.CloseHandle(proc)

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		logJobFallback(pid, "CreateJobObject", err)
		return fmt.Errorf("%w: CreateJobObject: %v", ErrJobUnavailable, err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			ActiveProcessLimit: uint32(maxProcessesPerCommand),
			LimitFlags:         windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		logJobFallback(pid, "SetInformationJobObject", err)
		windows.CloseHandle(job)
		return fmt.Errorf("%w: SetInformationJobObject: %v", ErrJobUnavailable, err)
	}
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		// Nesting under an existing job can be disallowed by the parent
		// job; that is a supported silent fallback, not a command error.
		logJobFallback(pid, "AssignProcessToJobObject", err)
		windows.CloseHandle(job)
		return fmt.Errorf("%w: AssignProcessToJobObject: %v", ErrJobUnavailable, err)
	}
	j.mu.Lock()
	j.handle = job
	j.mu.Unlock()
	return nil
}

func logJobFallback(pid int, step string, err error) {
	slog.Debug("Per-command job object unavailable; falling back to taskkill", "pid", pid, "step", step, "error", err)
}

// terminate kills every process currently in the job, including orphaned
// descendants whose parent already exited. This is the whole point of the
// job: taskkill /T can only walk a chain of LIVE parents, while a bash
// waiting on a command-substitution pipe hangs because an orphaned
// exec-chain holds the write end forever. Closing the handle without
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE kills nothing, so terminate is only
// called from an explicit kill path.
func (j *commandJob) terminate() {
	j.mu.Lock()
	h := j.handle
	j.mu.Unlock()
	if h != 0 {
		_ = windows.TerminateJobObject(h, 1)
	}
}

// close releases the job handle. Because the job has no
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE flag, this terminates nothing: a
// deliberately leaked daemon stays alive with the active-process limit
// still in force for as long as it lives.
func (j *commandJob) close() {
	j.mu.Lock()
	h := j.handle
	j.handle = 0
	j.mu.Unlock()
	if h != 0 {
		_ = windows.CloseHandle(h)
	}
}
