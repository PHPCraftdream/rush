package agent

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// batchBindingExecution holds a binding/leaf-registry lease across the
// accepted execution. The Runner invokes Dispose only after the terminal
// result has been reported (see jobbatch.Disposer); the idempotent release
// runs only after the wrapped family has physically disposed its handle.
type batchBindingExecution struct {
	jobbatch.Execution
	output    jobbatch.OutputReader
	messenger jobbatch.Messenger
	disposer  jobbatch.Disposer
	release   func()
	once      sync.Once
}

func (e *batchBindingExecution) Stop(ctx context.Context) error {
	return e.Execution.Stop(ctx)
}

// The base type deliberately advertises ONLY Execution + Disposer: ReadOutput
// and Inject live on the specialized wrappers below, so an execution without
// output/inject support no longer satisfies jobbatch.OutputReader/Messenger.

func (e *batchBindingExecution) Dispose(ctx context.Context) error {
	if e.disposer != nil {
		if err := e.disposer.Dispose(ctx); err != nil {
			return err
		}
	}
	// A nil disposer means a foreign execution without Disposer support;
	// Start already returned this execution, so the binding lease must
	// still be released.
	e.once.Do(func() {
		if e.release != nil {
			e.release()
		}
	})
	return nil
}

var (
	_ jobbatch.Execution    = (*batchBindingExecution)(nil)
	_ jobbatch.Disposer     = (*batchBindingExecution)(nil)
	_ jobbatch.Disposer     = (*batchBindingExecutionWithOutput)(nil)
	_ jobbatch.Disposer     = (*batchBindingExecutionWithMessenger)(nil)
	_ jobbatch.Disposer     = (*batchBindingExecutionWithBoth)(nil)
	_ jobbatch.OutputReader = (*batchBindingExecutionWithOutput)(nil)
	_ jobbatch.Messenger    = (*batchBindingExecutionWithMessenger)(nil)
	_ jobbatch.OutputReader = (*batchBindingExecutionWithBoth)(nil)
	_ jobbatch.Messenger    = (*batchBindingExecutionWithBoth)(nil)
)

func newBatchBindingExecution(execution jobbatch.Execution, release func()) jobbatch.Execution {
	wrapped := &batchBindingExecution{Execution: execution, release: release}
	supported := false
	if wrapped.disposer, supported = execution.(jobbatch.Disposer); !supported {
		wrapped.disposer = nil
	}
	if output, ok := execution.(jobbatch.OutputReader); ok {
		wrapped.output = output
	}
	if messenger, ok := execution.(jobbatch.Messenger); ok {
		wrapped.messenger = messenger
	}
	switch {
	case wrapped.output != nil && wrapped.messenger != nil:
		return &batchBindingExecutionWithBoth{wrapped}
	case wrapped.output != nil:
		return &batchBindingExecutionWithOutput{wrapped}
	case wrapped.messenger != nil:
		return &batchBindingExecutionWithMessenger{wrapped}
	default:
		return wrapped
	}
}

type (
	batchBindingExecutionWithOutput    struct{ *batchBindingExecution }
	batchBindingExecutionWithMessenger struct{ *batchBindingExecution }
	batchBindingExecutionWithBoth      struct{ *batchBindingExecution }
)

func (e *batchBindingExecutionWithOutput) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	return e.output.ReadOutput(ctx, cursor)
}

func (e *batchBindingExecutionWithMessenger) Inject(ctx context.Context, msg json.RawMessage) error {
	return e.messenger.Inject(ctx, msg)
}

func (e *batchBindingExecutionWithBoth) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	return e.output.ReadOutput(ctx, cursor)
}

func (e *batchBindingExecutionWithBoth) Inject(ctx context.Context, msg json.RawMessage) error {
	return e.messenger.Inject(ctx, msg)
}
