//go:build !windows

package session

import "syscall"

// Вызывается под trackedGroupsMu, но fast-stop этот mutex не ждёт.
func publishTrackedGroups() {
	pids := make([]int, 0, len(trackedGroups))
	for pid := range trackedGroups {
		pids = append(pids, pid)
	}
	trackedGroupsSnapshot.Store(&pids)
}

func stopTrackedProcessTrees() {
	if snapshot := trackedGroupsSnapshot.Load(); snapshot != nil {
		for _, pid := range *snapshot {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
}
