package batchexec

import (
	"context"
	"errors"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// NewGeneric creates an accepted execution without a retained process handle.
func NewGeneric(cancel context.CancelFunc, output Output) *GenericExecution {
	return &GenericExecution{output: output, cancel: cancel, done: make(chan struct{})}
}

type GenericExecution struct {
	output   Output
	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	stopped  bool
	finished bool
	disposed bool
	content  []byte
}

var (
	_ jobbatch.Execution    = (*GenericExecution)(nil)
	_ jobbatch.OutputReader = (*GenericExecution)(nil)
	_ jobbatch.Disposer     = (*GenericExecution)(nil)
)

// Run invokes the pinned tool once and reports only after completion and output handoff.
func (e *GenericExecution) Run(ctx context.Context, leaf Runner, call fantasy.ToolCall, report func(jobbatch.Result)) {
	resp, err, panicked := runGeneric(ctx, leaf, call)
	e.mu.Lock()
	cancelled := e.stopped && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
	result := e.output.Result(resp, err, cancelled)
	if panicked {
		result = jobbatch.Result{State: jobbatch.StateFailed, Code: "tool_panic"}
	}
	e.content = []byte(resp.Content)
	if e.output.Store != nil {
		e.output.Store(jobbatch.OutputChunk{Data: e.content, Next: uint64(len(e.content)), EOF: true})
	}
	e.finished = true
	close(e.done)
	e.mu.Unlock()
	e.cancel()
	report(result)
}

func runGeneric(ctx context.Context, leaf Runner, call fantasy.ToolCall) (resp fantasy.ToolResponse, err error, panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	resp, err = leaf.Run(ctx, call)
	return
}

func (e *GenericExecution) Stop(context.Context) error {
	e.mu.Lock()
	if !e.finished && !e.stopped {
		e.stopped = true
		e.cancel()
	}
	e.mu.Unlock()
	return nil
}

func (e *GenericExecution) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	select {
	case <-e.done:
	case <-ctx.Done():
		return jobbatch.OutputChunk{}, ctx.Err()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.disposed {
		return jobbatch.OutputChunk{}, jobbatch.ErrReleased
	}
	next := uint64(len(e.content))
	if cursor > next {
		cursor = next
	}
	return jobbatch.OutputChunk{Data: append([]byte(nil), e.content[cursor:]...), Next: next, Oldest: 0, EOF: true}, nil
}

func (e *GenericExecution) Dispose(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.finished {
		return jobbatch.ErrNotTerminal
	}
	e.content = nil
	e.disposed = true
	return nil
}
