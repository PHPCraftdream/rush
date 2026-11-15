package shell

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var (
	ErrExecutionDisposed = errors.New("shell execution disposed")
	ErrExecutionNotDone  = errors.New("shell execution is still running")
)

// BackgroundExecution retains completion and bounded output independently of
// manager lookup. Done means ExecStream returned, not proof that an escaped
// descendant tree is empty. Stop's result is control feedback, never Done.
// Dispose must follow Done; no handle calls may follow successful disposal.
type BackgroundExecution struct {
	shell    *BackgroundShell
	mu       sync.RWMutex
	disposed bool
	stopSlot chan struct{}
	physical *retainedStops
}

// StartRetainedOwned starts opt-in retained execution. Legacy launches keep
// their existing output retention and manager-control behavior.
func (m *BackgroundShellManager) StartRetainedOwned(ctx context.Context, sessionID, workingDir string, blockFuncs []BlockFunc, command, description string) (*BackgroundExecution, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session ID is required for background shell ownership")
	}
	physical := &retainedStops{entries: make(map[*retainedProcess]struct{}), output: newRetainedOutput(maxStreamBufferBytes)}
	ctx = context.WithValue(ctx, retainedStopKey{}, physical)
	bs, err := m.startWithRetention(ctx, sessionID, workingDir, blockFuncs, command, description, true)
	if err != nil {
		return nil, err
	}
	execution := newBackgroundExecution(bs)
	execution.physical = physical
	return execution, nil
}

func newBackgroundExecution(bs *BackgroundShell) *BackgroundExecution {
	return &BackgroundExecution{shell: bs, stopSlot: make(chan struct{}, 1)}
}

// ID is the legacy manager identity, not an additional execution identity.
func (e *BackgroundExecution) ID() string { return e.shell.ID }

// Done is the original ExecStream completion channel; control never closes it.
func (e *BackgroundExecution) Done() <-chan struct{} { return e.shell.done }

// Stop acknowledges physical kill dispatch for retained launches, without
// waiting for Done. Physical errors are returned separately from control-context
// errors while acquiring the dispatch slot. A dispatched kill runs to actual
// return; it is not abandoned at a control deadline. Hand-built legacy handles
// retain their cancellation-and-wait behavior.
func (e *BackgroundExecution) Stop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case e.stopSlot <- struct{}{}:
		defer func() { <-e.stopSlot }()
	case <-ctx.Done():
		return ctx.Err()
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.disposed {
		return ErrExecutionDisposed
	}
	if e.shell.IsDone() {
		return nil
	}
	if e.physical != nil {
		// Serialize physical kills with cancellation callbacks before cancelling
		// the interpreter. Neither acknowledgment nor error closes Done.
		return e.physical.stop()
	}
	e.shell.cancel()
	select {
	case <-e.shell.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// GetOutput preserves the legacy separate bounded snapshot semantics.
func (e *BackgroundExecution) GetOutput() (stdout, stderr string, done bool, err error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.disposed {
		return "", "", false, ErrExecutionDisposed
	}
	return e.shell.GetOutput()
}

// TotalWrittenBytes preserves the monotonic change-detection cursor.
func (e *BackgroundExecution) TotalWrittenBytes() (int, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.disposed {
		return 0, ErrExecutionDisposed
	}
	return e.shell.TotalWrittenBytes(), nil
}

// WaitForChange observes growth or real completion, without manager lookup.
func (e *BackgroundExecution) WaitForChange(ctx context.Context, sinceLen int) error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.disposed {
		return ErrExecutionDisposed
	}
	e.shell.WaitForChange(ctx, sinceLen)
	return ctx.Err()
}

// Dispose releases output once after completion and active handle calls return.
// An early or canceled attempt does not consume disposal. Repeated successful
// disposal is harmless. Context is checked before and after acquiring the
// call barrier; it does not bound that acquisition. An early call rejects
// without acquiring the barrier. Disposal neither cancels nor removes metadata.
func (e *BackgroundExecution) Dispose(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !e.shell.IsDone() {
		return ErrExecutionNotDone
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.disposed {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !e.shell.IsDone() {
		return ErrExecutionNotDone
	}
	e.disposed = true
	if e.physical != nil && e.physical.output != nil {
		e.physical.output.mu.Lock()
		e.physical.output.data = nil
		e.physical.output.mu.Unlock()
	}
	e.shell.retentionMu.Lock()
	e.shell.retained = false
	retentionTimer := e.shell.retentionTimer
	detachedTimer := e.shell.detachedReleaseTimer
	e.shell.retentionTimer = nil
	e.shell.detachedReleaseTimer = nil
	e.shell.retentionMu.Unlock()
	if retentionTimer != nil {
		retentionTimer.Stop()
	}
	if detachedTimer != nil {
		detachedTimer.Stop()
	}
	e.shell.releaseBuffers()
	return nil
}
