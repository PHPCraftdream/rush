//go:build windows

package session

// On Windows, cross-process CLI-child-tree teardown for sessions kill is
// already solved by the Job Object mechanism in track_windows.go
// (KILL_ON_JOB_CLOSE): TrackProcessTree assigns the child's whole future
// process tree to a job whose handle KillProcess (kill_windows.go)
// terminates directly by pid -- no separate cross-process handoff is
// needed because Windows process trees, unlike POSIX process groups,
// don't require the killer to know a second identifier (a pgid) ahead of
// time. These are no-op stubs so the platform-agnostic call sites in
// internal/agent/cliprovider (trackChildTree / the deferred cleanup in
// provider_stream.go) and internal/cmd/sessions_kill.go do not need
// Unix-only build tags of their own.

func RegisterChildGroup(dataDir, sessionID string, pgid int, generation string) {}

func UnregisterChildGroup(dataDir, sessionID string, pgid int) {}

func RemoveChildGroupRegistry(dataDir, sessionID string) {}

type ChildGroupSweepResult struct {
	Killed             int
	GenerationMismatch bool
	Implausible        int
}

func KillRegisteredChildGroups(dataDir, sessionID, lockPath string) ChildGroupSweepResult {
	return ChildGroupSweepResult{}
}
