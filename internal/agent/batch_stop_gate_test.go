package agent

// AW-b held-Stop gate (R12-E round 5): a TEST-ONLY jobbatch.Executor that
// wraps the DEFAULT batch executor (newBatchExecutor over the default
// families) so that every Execution's Stop blocks on a gate channel before
// delegating to the wrapped execution's Stop. Everything else forwards.
//
// Why: Runner.Stop returns on ACCEPTANCE and dispatches the executor Stop
// asynchronously (runner_control.go: runnerStopReturned is enqueued after
// execution.Stop returns; batch_exec_generic.go's generic Stop returns
// immediately). Without the gate the post-acceptance AW-b assertions race
// the dispatched Stop. With it, "the executor Stop has NOT returned" is
// deterministic until the test closes the gate.

import (
	"context"
	"encoding/json"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// stopGateExecutor wraps an Executor; every Start's Execution is wrapped so
// its Stop waits on gate.
type stopGateExecutor struct {
	jobbatch.Executor
	gate chan struct{}
}

var _ jobbatch.Executor = (*stopGateExecutor)(nil)

func (x *stopGateExecutor) Start(ctx context.Context, l jobbatch.Launch, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	exec, err := x.Executor.Start(ctx, l, report)
	if err != nil {
		return nil, err
	}
	return &gatedStopExecution{Execution: exec, gate: x.gate}, nil
}

// gatedStopExecution forwards every capability of the wrapped execution,
// overriding ONLY Stop.
type gatedStopExecution struct {
	jobbatch.Execution
	gate chan struct{}
}

var (
	_ jobbatch.Execution    = (*gatedStopExecution)(nil)
	_ jobbatch.OutputReader = (*gatedStopExecution)(nil)
	_ jobbatch.Messenger    = (*gatedStopExecution)(nil)
	_ jobbatch.Disposer     = (*gatedStopExecution)(nil)
)

// Stop blocks until the gate is released, then delegates. The underlying
// Stop runs on the CALLER's goroutine (the runner's dispatched stop worker)
// exactly as it would unwrapped.
func (e *gatedStopExecution) Stop(ctx context.Context) error {
	<-e.gate
	return e.Execution.Stop(ctx)
}

func (e *gatedStopExecution) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	if r, ok := e.Execution.(jobbatch.OutputReader); ok {
		return r.ReadOutput(ctx, cursor)
	}
	return jobbatch.OutputChunk{EOF: true}, nil
}

func (e *gatedStopExecution) Inject(ctx context.Context, msg json.RawMessage) error {
	if m, ok := e.Execution.(jobbatch.Messenger); ok {
		return m.Inject(ctx, msg)
	}
	return ErrBatchAgentNotHeld
}

func (e *gatedStopExecution) Dispose(ctx context.Context) error {
	if d, ok := e.Execution.(jobbatch.Disposer); ok {
		return d.Dispose(ctx)
	}
	return nil
}

// gatedExecutor builds the DEFAULT batch executor over the pinned registry
// (the same construction InstallBatchRuntime performs for a nil Executor,
// with the caller's agent deps) and applies the test's wrap. The install
// path builds its executor outside when one is supplied, so agent deps, their
// publisher and the App-owned capability manager must be wired by the env
// constructor (newCutoverEnvDeps): the executor's Execution owner is acquired
// in Start against THIS manager (R12-H2 §1.2).
func gatedExecutor(wrap func(jobbatch.Executor) jobbatch.Executor, registries *BatchLeafRegistryStore, agentDeps BatchAgentDeps, bindings *DiskProviderRegistry) jobbatch.Executor {
	base := newBatchExecutor(batchLeafEnv{Leaves: registries.Resolve, DiskBindings: bindings}, newBatchExecutorFamilies(agentDeps))
	if wrap == nil {
		return base
	}
	return wrap(base)
}
