package state

import (
	"context"
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// ExecutionLifecycle owns settlement, retained output, and physical disposal.
// The caller supplies the generation mutex shared with its request snapshot.
type ExecutionLifecycle struct {
	result                      jobbatch.Result
	content                     []byte
	reported, stopped, disposed bool
}

// Status returns the messaging gates in a single synchronized read.
func (e *ExecutionLifecycle) Status(mu *sync.Mutex) (disposed, reported, stopped bool) {
	mu.Lock()
	defer mu.Unlock()
	return e.disposed, e.reported, e.stopped
}

// BeginStopLocked records stop intent without settling the incarnation.
// The caller must hold the generation mutex through sampling the turn.
func (e *ExecutionLifecycle) BeginStopLocked() (first bool, err error) {
	if e.disposed {
		return false, jobbatch.ErrReleased
	}
	first = !e.stopped
	e.stopped = true
	return first, nil
}

// Finish retains the terminal summary and reports once, outside the lock.
func (e *ExecutionLifecycle) Finish(mu *sync.Mutex, finished chan struct{}, result jobbatch.Result, retain func(jobbatch.OutputChunk), report func(jobbatch.Result)) {
	mu.Lock()
	if e.reported {
		mu.Unlock()
		return
	}
	e.content = []byte(result.Summary)
	if retain != nil {
		retain(jobbatch.OutputChunk{Data: e.content, Next: uint64(len(e.content)), EOF: true})
	}
	e.reported = true
	e.result = result
	close(finished)
	mu.Unlock()
	report(result)
}

// ReadOutput returns a copy of retained output after settlement.
func (e *ExecutionLifecycle) ReadOutput(ctx context.Context, mu *sync.Mutex, finished <-chan struct{}, cursor uint64) (jobbatch.OutputChunk, error) {
	select {
	case <-finished:
	case <-ctx.Done():
		return jobbatch.OutputChunk{}, ctx.Err()
	}
	mu.Lock()
	defer mu.Unlock()
	if e.disposed {
		return jobbatch.OutputChunk{}, jobbatch.ErrReleased
	}
	next := uint64(len(e.content))
	if cursor > next {
		cursor = next
	}
	return jobbatch.OutputChunk{Data: append([]byte(nil), e.content[cursor:]...), Next: next, EOF: true}, nil
}

// Dispose waits for physical driver exit and watcher completion before release.
func (e *ExecutionLifecycle) Dispose(ctx context.Context, mu *sync.Mutex, finished, driverDone, watcherDone <-chan struct{}, handoff *toolwrap.PctDelegationHandoff) error {
	select {
	case <-finished:
	default:
		return jobbatch.ErrNotTerminal
	}
	select {
	case <-driverDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-watcherDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	mu.Lock()
	defer mu.Unlock()
	if e.disposed {
		return nil
	}
	e.disposed = true
	e.content = nil
	if handoff != nil {
		// Release the executor-owned handoff only after confirmed disposal.
		handoff.Release()
	}
	return nil
}

// StartExecutionWorkers tracks the watcher and the late driver exit separately.
func StartExecutionWorkers(internal *sync.WaitGroup, driverDone chan struct{}, turn interface{ Exit() <-chan struct{} }, watch func()) {
	internal.Add(2)
	go func() {
		defer internal.Done()
		watch()
	}()
	go func() {
		defer internal.Done()
		<-turn.Exit()
		close(driverDone)
	}()
}
