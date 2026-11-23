package batchexec

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell/shellbg"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// ShellHandle separates physical control from authoritative completion.
type ShellHandle interface {
	Stop(context.Context) error
	Done() <-chan struct{}
	GetOutput() (string, string, bool, error)
	ReadOutput(context.Context, uint64) (shellbg.ExecutionOutput, error)
	Dispose(context.Context) error
}

type ShellExecution struct {
	output    Output
	capture   func(jobbatch.OutputReader)
	mu        sync.RWMutex
	calls     sync.RWMutex
	handle    ShellHandle
	cancel    context.CancelFunc
	stopped   bool
	disposed  bool
	finished  chan struct{}
	published chan struct{}
}

var (
	_ jobbatch.Execution    = (*ShellExecution)(nil)
	_ jobbatch.OutputReader = (*ShellExecution)(nil)
	_ jobbatch.Disposer     = (*ShellExecution)(nil)
)

// NewShell creates an accepted execution. Capture, when set, receives the drained handle's reader before report.
func NewShell(cancel context.CancelFunc, output Output, capture func(jobbatch.OutputReader)) *ShellExecution {
	return &ShellExecution{output: output, capture: capture, cancel: cancel, finished: make(chan struct{}), published: make(chan struct{})}
}

// WithShellHandle marks the pinned tool call for retained shell ownership.
func (e *ShellExecution) WithShellHandle(ctx context.Context) context.Context {
	return tools.WithShellLeaf(ctx, tools.ShellLeaf{OnStart: func(h *shellbg.BackgroundExecution) { e.Publish(h) }})
}

// Publish transfers a retained shell to the execution. Call at most once.
func (e *ShellExecution) Publish(h ShellHandle) {
	e.mu.Lock()
	e.handle = h
	if e.published != nil {
		close(e.published)
	}
	e.mu.Unlock()
}

// Run invokes the pinned tool once and reports only after completion and output handoff.
func (e *ShellExecution) Run(ctx context.Context, leaf Runner, call fantasy.ToolCall, report func(jobbatch.Result)) {
	var resp fantasy.ToolResponse
	var err error
	func() {
		defer func() {
			if recover() != nil {
				err = errors.New("shell leaf panic")
			}
		}()
		resp, err = leaf.Run(ctx, call)
	}()
	e.mu.RLock()
	h := e.handle
	e.mu.RUnlock()
	// A panic/Run return cannot stand in for a live handle's real completion.
	if h != nil {
		<-h.Done()
	}
	e.mu.Lock()
	stopped := e.stopped
	result := e.output.Result(resp, err, stopped)
	if h != nil && !stopped {
		_, _, _, exitErr := h.GetOutput()
		if code := shell.ExitCode(exitErr); code != 0 {
			result.State = jobbatch.StateFailed
			result.Code = fmt.Sprintf("exit_%d", code)
		}
	}
	if h != nil {
		if e.capture != nil {
			e.capture(shellOutputReader{h})
		}
	} else if e.output.Store != nil {
		e.output.Store(jobbatch.OutputChunk{Data: []byte(resp.Content), Next: uint64(len(resp.Content)), EOF: true})
	}
	close(e.finished)
	e.mu.Unlock()
	e.cancel()
	report(result)
}

type shellOutputReader struct{ h ShellHandle }

func (r shellOutputReader) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	c, err := r.h.ReadOutput(ctx, cursor)
	return jobbatch.OutputChunk{Data: c.Data, Next: c.Next, Oldest: c.Oldest, EOF: c.EOF, Truncated: c.Truncated}, err
}

// Stop holds its physical-call barrier until actual return. Context bounds
// handle dispatch, not a running kill; Runner supplies logical control timeout.
func (e *ShellExecution) Stop(ctx context.Context) error {
	e.calls.RLock()
	defer e.calls.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	if e.disposed {
		e.mu.Unlock()
		return jobbatch.ErrReleased
	}
	e.stopped = true
	h := e.handle
	if h == nil {
		e.cancel()
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()
	// Cancel Run only after physical feedback, avoiding a callback winning and
	// hiding its error from this control invocation.
	err := h.Stop(ctx)
	e.cancel()
	return err
}

func (e *ShellExecution) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	e.calls.RLock()
	defer e.calls.RUnlock()
	e.mu.RLock()
	h, disposed := e.handle, e.disposed
	e.mu.RUnlock()
	if disposed {
		return jobbatch.OutputChunk{}, jobbatch.ErrReleased
	}
	if h == nil {
		return jobbatch.OutputChunk{}, jobbatch.ErrNotStarted
	}
	c, err := h.ReadOutput(ctx, cursor)
	return jobbatch.OutputChunk{Data: c.Data, Next: c.Next, Oldest: c.Oldest, EOF: c.EOF, Truncated: c.Truncated}, err
}

func (e *ShellExecution) Dispose(ctx context.Context) error {
	select {
	case <-e.finished:
	default:
		return shellbg.ErrExecutionNotDone
	}
	e.calls.Lock()
	defer e.calls.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.disposed {
		return nil
	}
	if e.handle != nil {
		if err := e.handle.Dispose(ctx); err != nil {
			return err
		}
	}
	e.disposed = true
	e.handle = nil
	return nil
}
