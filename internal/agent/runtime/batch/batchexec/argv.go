package batchexec

import (
	"context"
	"fmt"
	"math"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// Keep the retained process contract small; tests can hold real completion
// independently of a control acknowledgement without changing the tools seam.
type ArgvHandle interface {
	Done() <-chan struct{}
	Outcome() (tools.ArgvOutcome, error)
	Stop(context.Context) error
	ReadOutput(context.Context, int64) (jobbatch.OutputChunk, error)
	Dispose(context.Context) error
}

type ArgvExecution struct {
	output            Output
	capture           func(ArgvHandle)
	mu                sync.Mutex
	process           ArgvHandle
	changed, finished chan struct{}
	cancel            context.CancelFunc
	disposed          bool

	// A control answer is not completion. Serialize the result classification
	// with the caller's Stop answer, not with the physical kill goroutine.
	stopMu  sync.Mutex
	stopped bool
}

var (
	_ jobbatch.Execution    = (*ArgvExecution)(nil)
	_ jobbatch.OutputReader = (*ArgvExecution)(nil)
	_ jobbatch.Disposer     = (*ArgvExecution)(nil)
)

// NewArgv creates an accepted execution. Capture, when set, receives the drained process before report.
func NewArgv(cancel context.CancelFunc, output Output, capture func(ArgvHandle)) *ArgvExecution {
	return &ArgvExecution{output: output, capture: capture, cancel: cancel, changed: make(chan struct{}), finished: make(chan struct{})}
}

// WithArgvHandle marks the pinned tool call for retained process ownership.
func (e *ArgvExecution) WithArgvHandle(ctx context.Context) context.Context {
	return tools.WithArgvLeaf(ctx, &tools.ArgvLeaf{OnStart: func(p *tools.ArgvProcess) { e.Publish(p) }})
}

// Publish transfers a retained process to the execution. Call at most once.
func (e *ArgvExecution) Publish(p ArgvHandle) {
	e.mu.Lock()
	e.process = p
	close(e.changed)
	e.mu.Unlock()
}

// Run invokes the pinned tool once and reports only after completion and output handoff.
func (e *ArgvExecution) Run(ctx context.Context, leaf Runner, call fantasy.ToolCall, report func(jobbatch.Result)) {
	var resp fantasy.ToolResponse
	var err error
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("argv leaf panic: %v", v)
			// A wrapper may panic after publication while a real process lives.
			// Containment must still join the real drained Wait before reporting.
			e.cancel()
			e.mu.Lock()
			p := e.process
			e.mu.Unlock()
			if p != nil {
				_ = p.Stop(context.Background())
			}
		}
		e.mu.Lock()
		p := e.process
		e.mu.Unlock()
		if p != nil {
			<-p.Done()
		}
		e.stopMu.Lock()
		r := e.output.Result(resp, err, e.stopped)
		if p != nil && r.State != jobbatch.StateCancelled {
			if outcome, outcomeErr := p.Outcome(); outcomeErr == nil && outcome.ExitCode > 0 {
				r.State, r.Code = jobbatch.StateFailed, fmt.Sprintf("exit_%d", outcome.ExitCode)
			}
		}
		if p != nil {
			if e.capture != nil {
				e.capture(p)
			}
		} else if e.output.Store != nil {
			// Finished without a published process: the live reader's
			// finished-without-process branch returns an empty EOF chunk at
			// Next 0 (ArgvExecution.ReadOutput). Cache exactly that
			// shape under the argv restart contract — never resp.Content,
			// which reaches the model only via the task result/notice.
			e.output.Store(jobbatch.OutputChunk{EOF: true})
		}
		e.mu.Lock()
		close(e.finished)
		e.mu.Unlock()
		e.stopMu.Unlock()
		e.cancel()
		report(r)
	}()
	resp, err = leaf.Run(ctx, call)
}

func (e *ArgvExecution) Stop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.stopMu.Lock()
	defer e.stopMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	p := e.process
	finished := false
	select {
	case <-e.finished:
		finished = true
	default:
	}
	e.mu.Unlock()
	if finished {
		return nil
	}
	e.stopped = true
	if p == nil {
		// The leaf may race publication. Its cancellation path must stop and
		// join any process it starts; we still report only from Run + Done.
		e.cancel()
		return nil
	}
	return p.Stop(ctx)
}

func (e *ArgvExecution) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	for {
		e.mu.Lock()
		p, disposed := e.process, e.disposed
		finished := false
		select {
		case <-e.finished:
			finished = true
		default:
		}
		e.mu.Unlock()
		if disposed {
			return jobbatch.OutputChunk{}, tools.ErrArgvDisposed
		}
		if p != nil {
			if cursor > math.MaxInt64 {
				cursor = 0
			}
			return p.ReadOutput(ctx, int64(cursor))
		}
		if finished {
			return jobbatch.OutputChunk{EOF: true}, nil
		}
		select {
		case <-ctx.Done():
			return jobbatch.OutputChunk{}, ctx.Err()
		case <-e.changed:
		case <-e.finished:
		}
	}
}

func (e *ArgvExecution) Dispose(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.disposed {
		return tools.ErrArgvDisposed
	}
	select {
	case <-e.finished:
	default:
		return tools.ErrArgvNotDone
	}
	if e.process != nil {
		if err := e.process.Dispose(ctx); err != nil {
			return err
		}
	}
	e.disposed = true
	return nil
}
