package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/shell"
)

// batchShellHandle separates physical control from authoritative completion.
type batchShellHandle interface {
	Stop(context.Context) error
	Done() <-chan struct{}
	GetOutput() (string, string, bool, error)
	ReadOutput(context.Context, uint64) (shell.ExecutionOutput, error)
	Dispose(context.Context) error
}

type batchShellFamily struct{}

var _ batchLeafFamily = batchShellFamily{}

type batchShellExecution struct {
	mu        sync.RWMutex
	calls     sync.RWMutex
	handle    batchShellHandle
	cancel    context.CancelFunc
	stopped   bool
	disposed  bool
	finished  chan struct{}
	published chan struct{}
}

var (
	_ jobbatch.Execution    = (*batchShellExecution)(nil)
	_ jobbatch.OutputReader = (*batchShellExecution)(nil)
	_ jobbatch.Disposer     = (*batchShellExecution)(nil)
)

func (batchShellFamily) start(ctx context.Context, env batchLeafEnv, _ jobbatch.Launch, p batchLeafPayload, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.Tool != tools.BashToolName {
		return nil, &batchStartError{batchCodeBadPayload, errors.New("shell kind requires bash")}
	}
	leaf, err := env.resolveLeaf(p)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// R11F round-2 (P1-5c): the PINNED policy wins — rebuilt exclusively
	// from the payload's immutable policy transport.
	runCtx = batchLeafRunContext(runCtx, p)
	e := &batchShellExecution{cancel: cancel, finished: make(chan struct{}), published: make(chan struct{})}
	runCtx = tools.WithShellLeaf(runCtx, tools.ShellLeaf{OnStart: func(h *shell.BackgroundExecution) { e.mu.Lock(); e.handle = h; close(e.published); e.mu.Unlock() }})
	go e.run(runCtx, leaf, fantasy.ToolCall{ID: p.ToolCallID, Name: p.Tool, Input: string(p.Input)}, report)
	return e, nil
}

func (e *batchShellExecution) run(ctx context.Context, leaf fantasy.AgentTool, call fantasy.ToolCall, report func(jobbatch.Result)) {
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
	result := batchResultFromResponse(resp, err, stopped)
	if h != nil && !stopped {
		_, _, _, exitErr := h.GetOutput()
		if code := shell.ExitCode(exitErr); code != 0 {
			result.State = jobbatch.StateFailed
			result.Code = fmt.Sprintf("exit_%d", code)
		}
	}
	close(e.finished)
	e.mu.Unlock()
	e.cancel()
	report(result)
}

// Stop holds its physical-call barrier until actual return. Context bounds
// handle dispatch, not a running kill; Runner supplies logical control timeout.
func (e *batchShellExecution) Stop(ctx context.Context) error {
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

func (e *batchShellExecution) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	e.calls.RLock()
	defer e.calls.RUnlock()
	e.mu.RLock()
	h := e.handle
	disposed := e.disposed
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

func (e *batchShellExecution) Dispose(ctx context.Context) error {
	select {
	case <-e.finished:
	default:
		return shell.ErrExecutionNotDone
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
