package jobbatch

import (
	"encoding/json"
	"time"
)

// ID names a node. Roots are caller-supplied; descendants are deterministic
// ordinal paths such as "root.1.2". Consumers never parse IDs for ownership.
type ID string

// Mode is how a batch schedules its direct children.
type Mode string

const (
	ModeParallel   Mode = "parallel"
	ModeSequential Mode = "sequential"
)

// OnFail is a sequential batch's reaction to a failed child. Parallel
// batches require it empty; a sequential batch with it empty uses stop.
type OnFail string

const (
	OnFailStop     OnFail = "stop"
	OnFailContinue OnFail = "continue"
)

// Notify selects which settlements become notices. Absent means all.
type Notify string

const (
	NotifyEach Notify = "each"
	NotifyAll  Notify = "all"
)

// State is a node lifecycle state (design 3.1).
type State string

const (
	StatePending     State = "pending"
	StateStarting    State = "starting"
	StateRunning     State = "running"
	StateCancelling  State = "cancelling"
	StateCompleted   State = "completed"
	StateFailed      State = "failed"
	StateCancelled   State = "cancelled"
	StateTimedOut    State = "timed_out"
	StateInterrupted State = "interrupted"
	StateSkipped     State = "skipped"
)

// Terminal reports whether s is a final state.
func (s State) Terminal() bool {
	switch s {
	case StateCompleted, StateFailed, StateCancelled, StateTimedOut, StateInterrupted, StateSkipped:
		return true
	}
	return false
}

// Active reports whether a leaf in s holds an admission unit.
func (s State) Active() bool {
	switch s {
	case StateStarting, StateRunning, StateCancelling:
		return true
	}
	return false
}

// Reportable reports whether an executor may report s as a Result.State.
func (s State) Reportable() bool {
	switch s {
	case StateCompleted, StateFailed, StateCancelled, StateTimedOut, StateInterrupted:
		return true
	}
	return false
}

// Cause is the first latched reason a node is being cancelled.
type Cause string

const (
	CauseNone        Cause = ""
	CauseStop        Cause = "stop"
	CauseTimeout     Cause = "timeout"
	CauseHostClosing Cause = "host_closing"
	CauseStopOnFail  Cause = "stop_on_fail"
)

// NodeKind classifies a node. NodeView.Kind is task or batch; a Notice may
// additionally name a root (a batch without a parent).
type NodeKind string

const (
	KindTask  NodeKind = "task"
	KindBatch NodeKind = "batch"
	KindRoot  NodeKind = "root"
)

// TaskSpec describes one leaf.
type TaskSpec struct {
	Kind    string          `json:"kind"`
	Group   string          `json:"group,omitempty"`   // Empty: the default group "".
	Payload json.RawMessage `json:"payload,omitempty"` // Opaque to the engine; read-only once accepted.
	Timeout time.Duration   `json:"timeout,omitempty"` // Zero means no item timeout.
}

// BatchSpec describes a batch and its direct children.
type BatchSpec struct {
	Mode        Mode   `json:"mode"`
	OnFail      OnFail `json:"on_fail,omitempty"`
	Notify      Notify `json:"notify,omitempty"`
	MaxParallel int    `json:"max_parallel,omitempty"` // 0: no cap of its own.
	Items       []Spec `json:"items,omitempty"`
}

// Spec is exactly one of a task or a batch.
type Spec struct {
	Task  *TaskSpec  `json:"task,omitempty"`
	Batch *BatchSpec `json:"batch,omitempty"`
}

// Limits are the shared scheduling budgets of one engine.
type Limits struct {
	MaxConcurrent int            `json:"max_concurrent"`
	ByGroup       map[string]int `json:"by_group,omitempty"`
	MaxDepth      int            `json:"max_depth"`
	MaxNodes      int            `json:"max_nodes"` // Per root, nested batches included.
}

// Counts are leaf counts; a nested batch is never counted, only its leaves.
type Counts struct {
	Pending     int `json:"pending"`
	Active      int `json:"active"` // Starting, running and cancelling.
	Completed   int `json:"completed"`
	Failed      int `json:"failed"`
	Cancelled   int `json:"cancelled"`
	TimedOut    int `json:"timed_out"`
	Interrupted int `json:"interrupted"`
	Skipped     int `json:"skipped"`
}

// Outcome is what an executor reported (or a start failure produced) for a
// leaf. A zero State means no outcome yet. Details are never part of a view.
type Outcome struct {
	State   State  `json:"state,omitempty"`
	Code    string `json:"code,omitempty"`
	Summary string `json:"summary,omitempty"` // For explicit inspection; never journaled.
}

// ControlState is a leaf's stop-control gate for its current token.
type ControlState string

const (
	ControlNone    ControlState = "none"
	ControlPending ControlState = "pending"
	ControlAcked   ControlState = "acked"
	ControlFailed  ControlState = "failed"
)

// NodeView is an immutable copy of one node. Payload is never a view field.
type NodeView struct {
	ID      ID       `json:"id"`
	Root    ID       `json:"root"`
	Parent  ID       `json:"parent,omitempty"`
	Ordinal int      `json:"ordinal"`
	Kind    NodeKind `json:"kind"`

	// Batch nodes only.
	Mode        Mode   `json:"mode,omitempty"`
	OnFail      OnFail `json:"on_fail,omitempty"`
	Notify      Notify `json:"notify,omitempty"`
	MaxParallel int    `json:"max_parallel,omitempty"`

	// Task nodes only.
	Group          string       `json:"group,omitempty"`
	Token          uint64       `json:"token,omitempty"` // Current admission attempt.
	Attempts       int          `json:"attempts,omitempty"`
	Outcome        Outcome      `json:"outcome"`
	Control        ControlState `json:"control,omitempty"`
	ControlError   string       `json:"control_error,omitempty"` // Code while Control is failed.
	ControlPending bool         `json:"control_pending,omitempty"`

	State State `json:"state"`
	Cause Cause `json:"cause,omitempty"`
}

// BatchView is a consistent view of a batch at one transition: the batch,
// its leaf counts and every descendant in pre-order.
type BatchView struct {
	Sequence    uint64     `json:"sequence"`
	Node        NodeView   `json:"node"`
	Counts      Counts     `json:"counts"`
	Descendants []NodeView `json:"descendants,omitempty"`
}

// Summary is the small immutable aggregate returned by Batch.Wait.
type Summary struct {
	Root     ID     `json:"root"`
	State    State  `json:"state"`
	Counts   Counts `json:"counts"`
	Sequence uint64 `json:"sequence"` // The transition that settled the root.
}
