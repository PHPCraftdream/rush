// Process-wide fatal handler (decision D4) for every host kind: CLI
// `rush run`, the long-lived server, SDK hosts. An SDK host also exits the
// process on a Runner fault — it does not only return an error.

package app

import (
	"log/slog"
	"os"
	"time"
)

// BatchFaultExitCode is the process exit code on a Runner commit fault
// (decision D4). CLI, server and SDK hosts all use it.
const BatchFaultExitCode = 70

// batchFaultExit is the injectable production exit. Tests replace it; the
// package-level default is os.Exit. Because os.Exit cannot run in-process
// tests, only the subprocess test exercises this default indirectly.
var batchFaultExit = os.Exit

// handleFault is the fatal path: fence first, then a bounded containment
// wait, log, retain, exit 70 exactly once.
//
// Order matters for the fault/shutdown race: the fence is set BEFORE the
// containment wait, so any shutdown that starts after the fault was
// observed sees Fenced()==true and never releases the host lock or the
// writer. A shutdown already in flight when the fault fires observes the
// fence at its next release step and aborts without releasing (see
// DrainAndRelease). No retry, no reopening of a faulted writer, no
// synthetic settlements: the Runner has already contained everything; the
// process exits and R9 recovery sweeps treat the host as dead.
func (rt *BatchRuntime) handleFault(faultErr error) {
	// Fence first: from this instant no normal cleanup path may release.
	rt.fenced.Store(true)

	wait := rt.cfg.ContainmentWait
	if wait <= 0 {
		wait = DefaultBatchFaultContainment
	}
	after := rt.cfg.WaitFunc
	if after == nil {
		after = time.After
	}
	// Bounded containment wait: give in-flight logging, stdout flushing and
	// the Runner's own containment a moment. Never unbounded; never a drain.
	<-after(wait)

	slog.Error("Batch runtime: commit fault - exiting process",
		"error", faultErr, "exit_code", BatchFaultExitCode,
		"containment", wait.String())

	// Retain, don't release: forget the in-memory host-lock handle the same
	// way the forced shutdown path does (CloseKeepLock), leaving the OS lock
	// and the writer untouched for the process's remaining life.
	rt.retainHostIdentity()

	rt.exitOnce.Do(func() {
		exit := rt.cfg.ExitFunc
		if exit == nil {
			exit = batchFaultExit
		}
		exit(BatchFaultExitCode)
	})
}

// retainHostIdentity applies the injected retention hook, nil-safe.
func (rt *BatchRuntime) retainHostIdentity() {
	if rt.cfg.RetainHostIdentity != nil {
		rt.cfg.RetainHostIdentity()
	}
}
