//go:build windows

package machinelock

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	"golang.org/x/sys/windows"
)

const stillActive = 259

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return false
	}
	return code == stillActive
}
func configureProcess(cmd *exec.Cmd) {}
func processCounters(pid int) uint64 {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)
	var creation, exit, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &creation, &exit, &kernel, &user) != nil {
		return 0
	}
	kernelTicks := uint64(kernel.HighDateTime)<<32 | uint64(kernel.LowDateTime)
	userTicks := uint64(user.HighDateTime)<<32 | uint64(user.LowDateTime)
	return kernelTicks + userTicks
}

func processSnapshot(pid int, record Record, started time.Time) string {
	state := "running"
	if !processAlive(pid) {
		state = "exited"
	}
	return fmt.Sprintf("Tree: PID %d state=%s age=%s command=%q checkout=%q\nWindows process-tree enumeration, child exit codes, and thread wait reasons are not implemented; CPU counters cover the root only.", pid, state, time.Since(started).Round(time.Second), record.Command, record.Checkout)
}

func gracefulStop(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = platform.Command(context.Background(), "taskkill", "/PID", fmt.Sprint(cmd.Process.Pid)).Run()
	}
}

func forceStop(cmd *exec.Cmd) {
	if cmd.Process != nil {
		forceStopPID(cmd.Process.Pid)
	}
}

// forceStopPID kills the process tree of pid (also used by tests to clean up a holder).
func forceStopPID(pid int) {
	_ = platform.Command(context.Background(), "taskkill", "/F", "/T", "/PID", fmt.Sprint(pid)).Run()
}
