//go:build !windows

package cmd

import (
	"os"
	"os/signal"
	"syscall"
)

// keepBrokenPipeAsError makes a write to a closed stdout/stderr pipe return
// EPIPE instead of killing the process (Go's default for fds 1 and 2: exit by
// SIGPIPE, status 141, at the final envelope write). The run's exit flush then
// fails through its ordinary error path, so --on-finish and App.Shutdown
// still run (R8C-7). signal.Notify, not Ignore: an ignored disposition is
// inherited by every spawned job, a handled one is reset on exec, so a job's
// `yes | head` keeps dying quietly.
func keepBrokenPipeAsError() (restore func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGPIPE)
	return func() { signal.Stop(c) }
}
