package jobbatch

import (
	"context"
	"encoding/json"
)

// Result is what an executor reports once an execution has stopped.
type Result struct {
	State   State           `json:"state"`             // Completed, Failed, Cancelled, TimedOut, Interrupted.
	Code    string          `json:"code,omitempty"`    // Short machine code; safe to journal.
	Summary string          `json:"summary,omitempty"` // For explicit inspection only; never journaled.
	Details json.RawMessage `json:"details,omitempty"` // Opaque; never logged automatically.
}

// Launch is one admission attempt of a leaf. It is read-only shared data: an
// executor must not mutate it, including Task.Payload.
type Launch struct {
	ID    ID       `json:"id"`
	Token uint64   `json:"token"` // Unique within one Runner.
	Task  TaskSpec `json:"task"`
}

// Executor owns actual execution and its output.
//
// Start must honor ctx cancellation and finish its setup in bounded time. A
// non-nil error means no execution exists. report states that the execution
// has stopped; it must return without waiting for the event loop, including
// when called synchronously inside Start.
type Executor interface {
	Start(ctx context.Context, l Launch, report func(Result)) (Execution, error)
}

// Execution is an accepted execution. Stop may overlap ReadOutput, Inject
// and report, but never another Stop on the same Execution.
type Execution interface {
	Stop(ctx context.Context) error
}

// OutputReader is an optional Execution capability.
type OutputReader interface {
	ReadOutput(ctx context.Context, cursor uint64) (OutputChunk, error)
}

// Messenger is an optional Execution capability.
type Messenger interface {
	Inject(ctx context.Context, msg json.RawMessage) error
}

// Disposer is an optional Execution capability. The Runner calls it once,
// after termination is reported and the handle is given up.
type Disposer interface {
	Dispose(ctx context.Context) error
}
