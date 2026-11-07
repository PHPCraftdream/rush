//go:build windows

package session

import "golang.org/x/sys/windows"

func stopTrackedProcessTrees() {
	// При занятом реестре деревья закрывает корневой kill-on-close Job CLI.
	if !trackedJobsMu.TryLock() {
		return
	}
	jobs := make([]windows.Handle, 0, len(trackedJobs))
	for _, job := range trackedJobs {
		jobs = append(jobs, job.handle)
	}
	trackedJobs = make(map[int]trackedJob)
	trackedJobsMu.Unlock()
	for _, job := range jobs {
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(job)
	}
}
