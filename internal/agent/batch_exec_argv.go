package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// batchArgvFamily is inert until explicitly registered with a batchExecutor.
// It uses the pinned policy leaf, never a raw run_command factory.
type batchArgvFamily struct{}

var _ batchLeafFamily = batchArgvFamily{}

// Keep the retained process contract small; tests can hold real completion
// independently of a control acknowledgement without changing the tools seam.
type batchArgvHandle interface {
	Done() <-chan struct{}
	Outcome() (tools.ArgvOutcome, error)
	Stop(context.Context) error
	ReadOutput(context.Context, int64) (jobbatch.OutputChunk, error)
	Dispose(context.Context) error
}

type batchArgvExecution struct {
	cache             *batchOutputCache
	nodeID, rootID    jobbatch.ID
	mu                sync.Mutex
	process           batchArgvHandle
	changed, finished chan struct{}
	cancel            context.CancelFunc
	disposed          bool

	// A control answer is not completion. Serialize the result classification
	// with the caller's Stop answer, not with the physical kill goroutine.
	stopMu  sync.Mutex
	stopped bool
}

var (
	_ jobbatch.Execution    = (*batchArgvExecution)(nil)
	_ jobbatch.OutputReader = (*batchArgvExecution)(nil)
	_ jobbatch.Disposer     = (*batchArgvExecution)(nil)
)

func (batchArgvFamily) start(ctx context.Context, env batchLeafEnv, l jobbatch.Launch, p batchLeafPayload, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.Tool != tools.RunCommandToolName {
		return nil, &batchStartError{batchCodeBadPayload, errors.New("argv requires run_command")}
	}
	leaf, err := env.resolveLeaf(p)
	if err != nil {
		return nil, err
	}
	if leaf == nil || leaf.Info().Name != tools.RunCommandToolName {
		return nil, &batchStartError{batchCodeNoLeaf, errors.New("pinned argv leaf is not run_command")}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Acceptance is the boundary: subsequent caller cancellation cannot reject
	// setup and abandon a process. Values survive, lifetime belongs to Stop.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// R11F round-2 (P1-5c): the PINNED policy wins — rebuilt exclusively
	// from the payload's immutable policy transport.
	runCtx = batchLeafRunContext(runCtx, p)
	e := &batchArgvExecution{cache: env.OutputCache, nodeID: l.ID, rootID: batchOutputRoot(l.ID), changed: make(chan struct{}), finished: make(chan struct{}), cancel: cancel}
	runCtx = tools.WithArgvLeaf(runCtx, &tools.ArgvLeaf{OnStart: func(p *tools.ArgvProcess) { e.publish(p) }})
	go e.run(runCtx, leaf, fantasy.ToolCall{ID: p.ToolCallID, Name: p.Tool, Input: string(p.Input)}, report)
	return e, nil
}

func (e *batchArgvExecution) publish(p batchArgvHandle) {
	e.mu.Lock()
	e.process = p
	close(e.changed)
	e.mu.Unlock()
}

func (e *batchArgvExecution) run(ctx context.Context, leaf fantasy.AgentTool, call fantasy.ToolCall, report func(jobbatch.Result)) {
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
		r := batchResultFromResponse(resp, err, e.stopped)
		if p != nil && r.State != jobbatch.StateCancelled {
			if outcome, outcomeErr := p.Outcome(); outcomeErr == nil && outcome.ExitCode > 0 {
				r.State, r.Code = jobbatch.StateFailed, fmt.Sprintf("exit_%d", outcome.ExitCode)
			}
		}
		if p != nil {
			_ = batchOutputCapture(e.cache, e.nodeID, e.rootID, batchArgvOutputReader{p})
		} else if e.cache != nil {
			e.cache.putFor(e.nodeID, e.rootID, jobbatch.OutputChunk{Data: []byte(resp.Content), Next: uint64(len(resp.Content)), EOF: true})
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

func (e *batchArgvExecution) Stop(ctx context.Context) error {
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

func (e *batchArgvExecution) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
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

func (e *batchArgvExecution) Dispose(ctx context.Context) error {
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
