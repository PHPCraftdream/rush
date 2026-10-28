package app

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

type batchFaultEnvelopeWriter struct {
	mu           sync.Mutex
	writer       io.Writer
	normalWrite  bool
	faultEmitted bool
}

// Write passes normal output through immediately and records that an envelope
// has been emitted so a later fault cannot produce a second one.
func (w *batchFaultEnvelopeWriter) Write(p []byte) (int, error) {
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

func (w *batchFaultEnvelopeWriter) writeFault(sessionID string, fault error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.normalWrite || w.faultEmitted {
		return nil
	}
	w.faultEmitted = true
	result, err := json.Marshal(batchFaultRunResult(sessionID, fault))
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

func batchFaultRunResult(sessionID string, fault error) *RunResult {
	text := "batch runner fault"
	if fault != nil {
		text = fault.Error()
	}
	return &RunResult{SessionID: sessionID, ExitReason: "batch_fault", Error: text, ToolCalls: []ToolCallStat{}, FinalText: "", Usage: UsageInfo{}}
}

func (app *App) handleBatchFaultOutput(fault error) {
	app.batchFaultOutputMu.Lock()
	write := app.batchFaultOutput
	app.batchFaultOutputMu.Unlock()
	if write != nil {
		write(fault)
	}
}

func writeBatchFaultOutput(stderr io.Writer, fault error) {
	text := "batch runner fault"
	if fault != nil {
		text = fault.Error()
	}
	fmt.Fprintf(stderr, "rush run: batch runtime fault: %s; exiting with code %d\n", text, BatchFaultExitCode)
}

var _ io.Writer = (*batchFaultEnvelopeWriter)(nil)
