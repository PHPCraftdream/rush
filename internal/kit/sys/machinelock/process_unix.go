//go:build !windows

package machinelock

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func configureProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func processCounters(pid int) uint64 { return treeCounter(pid) }

func processSnapshot(pid int, record Record, started time.Time) string {
	out, err := exec.CommandContext(context.Background(), "ps", "-eo", "pid=,ppid=,stat=,etime=,time=,args=").Output()
	if err != nil {
		return fmt.Sprintf("PID %d state=unknown age=%s command=%q checkout=%q; CPU/I/O counters unavailable, using conservative wall-clock fallback.", pid, time.Since(started).Round(time.Second), record.Command, record.Checkout)
	}
	selected := descendantPIDs(pid, out)
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		p, _ := strconv.Atoi(fields[0])
		if selected[p] {
			lines = append(lines, fmt.Sprintf("PID %s state=%s age=%s cpu=%s command=%s", fields[0], fields[2], fields[3], fields[4], strings.TrimSpace(line)))
		}
	}
	return fmt.Sprintf("Tree (exit codes are lost once anything is killed; portable ps cannot recover exited-child codes):\n%s\nHolder command=%q checkout=%q; CPU/I/O counters unavailable, using conservative wall-clock fallback.", strings.Join(lines, "\n"), record.Command, record.Checkout)
}

func treeCounter(pid int) uint64 {
	out, err := exec.CommandContext(context.Background(), "ps", "-eo", "pid=,ppid=,time=").Output()
	if err != nil {
		return 0
	}
	selected := descendantPIDs(pid, out)
	var total uint64
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		p, _ := strconv.Atoi(fields[0])
		if selected[p] {
			total += cpuSeconds(fields[2])
		}
	}
	return total
}

func descendantPIDs(root int, out []byte) map[int]bool {
	parents := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, e1 := strconv.Atoi(fields[0])
		parent, e2 := strconv.Atoi(fields[1])
		if e1 == nil && e2 == nil {
			parents[pid] = parent
		}
	}
	selected, changed := map[int]bool{root: true}, true
	for changed {
		changed = false
		for pid, parent := range parents {
			if selected[parent] && !selected[pid] {
				selected[pid], changed = true, true
			}
		}
	}
	return selected
}

func cpuSeconds(s string) uint64 {
	parts := strings.Split(s, ":")
	var seconds uint64
	for _, part := range parts {
		n, _ := strconv.ParseUint(part, 10, 64)
		seconds = seconds*60 + n
	}
	return seconds
}

func gracefulStop(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}

func forceStop(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
