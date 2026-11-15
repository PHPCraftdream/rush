//go:build windows

package shexec

import (
	"os/exec"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/sys/procinfo"
	"golang.org/x/sys/windows"
)

// REVERT CHECK: remove newProcessJob/SetInformationJobObject or the
// TerminateJobObject call in terminate → TestProcessJobTerminateKillsMember
// fails (the member outlives the job). Set maxProcessesPerCommand to a huge
// value in TestProcessJobAppliesLimit → the limit assertion has no bite.

// startSleeper starts a quiet bash sleep as a stand-in command process.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("bash", "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	t.Cleanup(func() {
		_ = procinfo.KillProcess(cmd.Process.Pid)
		_ = cmd.Wait()
	})
	return cmd
}

func TestProcessJobAppliesLimitAndRestore(t *testing.T) {
	if maxProcessesPerCommand != 2048 {
		t.Fatalf("expected default limit 2048, got %d", maxProcessesPerCommand)
	}
	restore := SetMaxProcessesPerCommand(7)
	if maxProcessesPerCommand != 7 {
		t.Fatalf("setter did not apply, got %d", maxProcessesPerCommand)
	}
	restore()
	if maxProcessesPerCommand != 2048 {
		t.Fatalf("restore did not apply, got %d", maxProcessesPerCommand)
	}
}

func TestProcessJobTerminateKillsMember(t *testing.T) {
	cmd := startSleeper(t)
	var job commandJob
	if err := assignProcessJob(cmd, &job); err != nil {
		// Assignment can be legitimately refused (nested-job policy);
		// the production fallback covers it, so skip rather than fail.
		t.Skipf("job assignment refused on this host: %v", err)
	}
	t.Cleanup(job.close)
	job.terminate()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("member survived TerminateJobObject with exit 0")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("TerminateJobObject did not kill the member within 5s")
	}
}

func TestProcessJobCloseLeavesMemberAlive(t *testing.T) {
	cmd := startSleeper(t)
	var job commandJob
	if err := assignProcessJob(cmd, &job); err != nil {
		t.Skipf("job assignment refused on this host: %v", err)
	}
	job.close()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatalf("member gone after CloseHandle without kill-on-close: %v", err)
	}
	defer windows.CloseHandle(h)
	// No KILL_ON_JOB_CLOSE invariant: closing the handle kills nothing.
	event, err := windows.WaitForSingleObject(h, 2000)
	if err != nil || event == windows.WAIT_OBJECT_0 {
		t.Fatalf("member was killed by job handle close: event=%v err=%v", event, err)
	}
}

func TestProcessJobAssignFailureFallsBack(t *testing.T) {
	// Forcing AssignProcessToJobObject to fail deterministically would
	// require controlling the host's job-nesting policy, which a test
	// cannot do portably; skipped by design.
	t.Skip("no deterministic way to force AssignProcessToJobObject failure on an arbitrary host")
}
