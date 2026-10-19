package app

// R11F round-2 P1-6 test: the Runner is constructed LAZILY at Start through
// BatchRuntimeConfig.RunnerFactory — a factory-built runtime has no Runner
// (no engine goroutines) until Start. Compile-only in this worktree (CI runs
// it); it fails against the pre-fix mutation that constructed the Runner at
// install time.

import (
	"errors"
	"testing"
	"time"
)

// TestBatchRuntimeRunnerFactoryAtStart (P1-6): no Runner before Start; Start
// constructs exactly once (first-wins) and attaches it; a factory error keeps
// the runtime runnerless (fail closed) without panicking.
func TestBatchRuntimeRunnerFactoryAtStart(t *testing.T) {
	constructed := 0
	build := func() (batchRunner, error) {
		constructed++
		return newFakeRunner(), nil
	}
	rt := NewBatchRuntime(BatchRuntimeConfig{
		ContainmentWait: time.Second,
		WaitFunc:        immediateWaitFunc(),
		ExitFunc:        func(int) {},
		RunnerFactory:   build,
	})
	if rt.Runner() != nil {
		t.Fatalf("Runner constructed before Start (P1-6: factory builds must stay dormant until Start)")
	}
	rt.Start()
	t.Cleanup(rt.Stop)
	if rt.Runner() == nil {
		t.Fatalf("Start did not construct the Runner through the factory")
	}
	if constructed != 1 {
		t.Fatalf("RunnerFactory called %d times, want exactly 1 (idempotent Start)", constructed)
	}
	rt.Start() // idempotent
	if constructed != 1 {
		t.Fatalf("second Start re-constructed the Runner (%d times)", constructed)
	}
}

// TestBatchRuntimeRunnerFactoryErrorStaysRunnerless (P1-6): a construction
// error is survivable — the runtime stays runnerless and later calls (drain,
// Invoke at the ingress boundary) fail closed.
func TestBatchRuntimeRunnerFactoryErrorStaysRunnerless(t *testing.T) {
	rt := NewBatchRuntime(BatchRuntimeConfig{
		ContainmentWait: time.Second,
		WaitFunc:        immediateWaitFunc(),
		ExitFunc:        func(int) {},
		RunnerFactory: func() (batchRunner, error) {
			return nil, errors.New("boom")
		},
	})
	rt.Start()
	t.Cleanup(rt.Stop)
	if rt.Runner() != nil {
		t.Fatalf("a failed construction must not attach a Runner")
	}
	if rt.Fenced() {
		t.Fatalf("a construction error is not an engine fault fence")
	}
}
