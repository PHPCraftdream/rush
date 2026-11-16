package appbatch

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Process-wide fatal handler (decision D4) for every host kind: CLI
// `rush run`, the long-lived server, SDK hosts. An SDK host also exits the
// process on a Runner fault — it does not only return an error.

// BatchFaultExitCode is the process exit code on a Runner commit fault
// (decision D4). CLI, server and SDK hosts all use it.
const BatchFaultExitCode = 70

// batchFaultExit is the injectable production exit. Tests replace it; the
// package-level default is os.Exit. Because os.Exit cannot run in-process
// tests, only the subprocess test exercises this default indirectly.
var batchFaultExit = os.Exit

// HandleFault is the fatal path: fence first, then a bounded containment
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
func (rt *BatchRuntime) HandleFault(faultErr error) {
	// Fence first: from this instant no normal cleanup path may release.
	rt.fenced.Store(true)
	diagnostics := rt.CommitDiagnostics.Snapshot(faultErr)

	wait := rt.Config.ContainmentWait
	if wait <= 0 {
		wait = DefaultBatchFaultContainment
	}
	after := rt.Config.WaitFunc
	if after == nil {
		after = time.After
	}
	// Bounded containment wait: give in-flight logging, stdout flushing and
	// the Runner's own containment a moment. Never unbounded; never a drain.
	<-after(wait)

	attrs := append([]any{
		"error", BatchCommitSafeFaultText(faultErr), "exit_code", BatchFaultExitCode,
		"containment", wait.String(),
	}, diagnostics...)
	slog.Error("Batch runtime: commit fault - exiting process", attrs...)

	// Retain, don't release: forget the in-memory host-lock handle the same
	// way the forced shutdown path does (CloseKeepLock), leaving the OS lock
	// and the writer untouched for the process's remaining life.
	rt.retainHostIdentity()

	rt.exitOnce.Do(func() {
		WriteBatchFaultOutput(os.Stderr, faultErr)
		if rt.Config.BeforeExit != nil {
			rt.Config.BeforeExit(faultErr)
		}
		exit := rt.Config.ExitFunc
		if exit == nil {
			exit = batchFaultExit
		}
		exit(BatchFaultExitCode)
	})
}

// retainHostIdentity applies the injected retention hook, nil-safe.
func (rt *BatchRuntime) retainHostIdentity() {
	if rt.Config.RetainHostIdentity != nil {
		rt.Config.RetainHostIdentity()
	}
}

type FaultEnvelopeWriter struct {
	mu           sync.Mutex
	writer       io.Writer
	faultResult  func(sessionID string, fault error) any
	normalWrite  bool
	faultEmitted bool
}

// NewFaultEnvelopeWriter builds the batch-fault envelope writer over an
// underlying stream. faultResult builds the per-fault result payload; it
// is injected because the run-result protocol types live above appbatch.
func NewFaultEnvelopeWriter(writer io.Writer, faultResult func(sessionID string, fault error) any) *FaultEnvelopeWriter {
	return &FaultEnvelopeWriter{writer: writer, faultResult: faultResult}
}

// Write passes normal output through immediately and records that an envelope
// has been emitted so a later fault cannot produce a second one.
func (w *FaultEnvelopeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.faultEmitted {
		return len(p), nil
	}
	n, err := w.writer.Write(p)
	if n > 0 {
		w.normalWrite = true
	}
	return n, err
}

func (w *FaultEnvelopeWriter) WriteFault(sessionID string, fault error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.normalWrite || w.faultEmitted {
		return nil
	}
	w.faultEmitted = true
	result, err := json.Marshal(w.faultResult(sessionID, fault))
	if err != nil {
		return err
	}
	return writeEnvelopeOnce(w.writer, append(result, '\n'))
}

// An underlying writer may accept a prefix and then fail. Marking the writer
// written before the fault write prevents retries appending a second envelope.
func writeEnvelopeOnce(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return err
}

func HandleBatchFaultOutput(p BatchCLIParams, fault error) {
	write := p.GetBatchFaultOutput()
	if write != nil {
		write(fault)
	}
}

func WriteBatchFaultOutput(stderr io.Writer, fault error) {
	text := "batch runner fault"
	if fault != nil {
		text = fault.Error()
	}
	fmt.Fprintf(stderr, "rush run: batch runtime fault: %s; exiting with code %d\n", text, BatchFaultExitCode)
}

var _ io.Writer = (*FaultEnvelopeWriter)(nil)
