//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package lab

import (
	"syscall"
	"testing"
	"time"
)

func processTestWaitPID(t *testing.T, pid int) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	ctx := processTestContext(t)
	for processTestAlive(pid) {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("PID %d still exists", pid)
		}
	}
	t.Logf("PID %d confirmed gone", pid)
}

func processTestMember(t *testing.T, p *processExecution, pid int) {
	t.Helper()
	group, err := syscall.Getpgid(pid)
	if err != nil || group != p.cmd.Process.Pid {
		t.Fatalf("PID %d outside process group: %d %v", pid, group, err)
	}
}

func processTestChildren(t *testing.T) map[int]bool { t.Helper(); return nil }

func processTestAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }
func processTestKillPID(pid int)    { _ = syscall.Kill(pid, syscall.SIGKILL) }
