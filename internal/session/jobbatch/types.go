// Package jobbatch is a standalone engine and runtime for batches of tasks.
//
// Every invocation is an ordinary batch; a one-task call uses OneItem. The
// design is docs/plans/2026-10-07-job-batches-design.md (revision 4); this
// package depends only on the standard library.
//
// # Frozen contract (phase A0)
//
// types.go, events.go, executor.go, runtime_types.go and errors.go hold the
// shared data declarations and a few pure helpers every slice must answer
// identically (State.Terminal/Active/Reportable, ValidCode, ReservedCode,
// ValidRef). Only the integration owner changes them. A slice that needs a
// missing field reports it instead of editing a contract file.
//
// The function and method signatures below are frozen by the design (section
// 2) and declared exactly once, with a real body, by the owning slice:
//
//	Core     (engine.go, tree.go, tree_apply.go, admission.go, view.go, notice.go)
//	  NewEngine(Limits) (*Engine, error)
//	  (*Engine) Apply(Event) (Transition, error)
//	  (*Engine) View(ID) (NodeView, bool)
//	  (*Engine) Snapshot(ID) (BatchView, bool)
//
//	Runtime  (runner.go, runner_dispatch.go, runner_control.go,
//	          runner_observe.go, runner_requests.go)
//	  OneItem(TaskSpec) Spec
//	  NewRunner(context.Context, RunnerConfig) (*Runner, error)
//	  (*Runner) Submit, Add, Stop, CapacityAvailable, View, Snapshot,
//	            Events, Output, Inject, Release, Close
//	  (*Batch) ID, Done, Wait
//
// Slice-owned package-level test helpers carry the slice prefix: coreTest*
// (testdata/core/) and runtimeTest* (testdata/runtime/). The laboratory
// (lab/) and cmd/batchlab consume this package; it never imports them.
package jobbatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// RunnerConfig configures one Runner.
type RunnerConfig struct {
	CommitHook      func(context.Context, Transition) error
	CommitTimeout   time.Duration // Positive when CommitHook is set.
	Limits          Limits
	Executor        Executor
	ControlTimeout  time.Duration // Per physical Stop/Dispose call, from dispatch; positive.
	RecordRetention int           // Records kept for Events; positive.
}

// SubmitRequest submits a root. Root must be a batch.
type SubmitRequest struct {
	Root ID
	Spec Spec
	Ref  string // Optional caller reference; journaled, never payload.
}

// AddRequest appends items to a nonterminal batch.
type AddRequest struct {
	Parent ID
	Items  []Spec
	Ref    string
}

// OutputChunk is one read of an execution's output stream.
type OutputChunk struct {
	Data      []byte `json:"data,omitempty"` // A caller-owned copy.
	Next      uint64 `json:"next"`
	Oldest    uint64 `json:"oldest"`
	EOF       bool   `json:"eof,omitempty"`
	Truncated bool   `json:"truncated,omitempty"` // The cursor was older than Oldest.
}

// RecordType names a public feed record.
type RecordType string

const (
	RecordSubmit            RecordType = "submit"
	RecordAdd               RecordType = "add"
	RecordStopRequested     RecordType = "stop_requested"
	RecordStarted           RecordType = "started"
	RecordStartRejected     RecordType = "start_rejected"
	RecordSettled           RecordType = "settled"
	RecordControl           RecordType = "control"
	RecordCapacityAvailable RecordType = "capacity_available"
	RecordDeadlineExpired   RecordType = "deadline_expired"
	RecordHostClosing       RecordType = "host_closing"
	RecordRelease           RecordType = "release"
	RecordCommand           RecordType = "command"
	RecordNotice            RecordType = "notice"
	RecordClosed            RecordType = "closed"
)

// Record is one public feed entry: metadata only, never a payload, Summary,
// Details, message, output, argument or error text. Fields not meaningful
// for a type are zero.
//
//	event records   Root/Node/Token/State/Cause/Reason/Code/Group/Ref as the
//	                event carries them; submit and add list the created IDs
//	                in IDs (add: Node is the parent)
//	command         Command, Node, Token, Cause
//	notice          Root, Node, Kind, State, Cause, Counts
//	closed          the last record, with Sequence S_F and Cursor F
type Record struct {
	Cursor   uint64       `json:"cursor"`
	Sequence uint64       `json:"sequence"`
	Type     RecordType   `json:"type"`
	Root     ID           `json:"root,omitempty"`
	Node     ID           `json:"node,omitempty"`
	Token    uint64       `json:"token,omitempty"`
	State    State        `json:"state,omitempty"`
	Cause    Cause        `json:"cause,omitempty"`
	Reason   RejectReason `json:"reason,omitempty"`
	Op       ControlOp    `json:"op,omitempty"`
	Code     string       `json:"code,omitempty"`
	Group    string       `json:"group,omitempty"`
	Ref      string       `json:"ref,omitempty"`
	IDs      []ID         `json:"ids,omitempty"`
	Command  CommandType  `json:"command,omitempty"`
	Kind     NodeKind     `json:"kind,omitempty"`
	Counts   *Counts      `json:"counts,omitempty"`
}

// EventPage is one read of the public feed. Cursors start at 1. Head is the
// inclusive newest cursor (0 when empty); Oldest is the oldest retained
// (Head+1 when empty); Next is the last returned cursor plus one. Final is 0
// until the Runner is Closed, then F forever; Closed iff Final != 0.
type EventPage struct {
	Records []Record `json:"records,omitempty"`
	Next    uint64   `json:"next"`
	Oldest  uint64   `json:"oldest"`
	Head    uint64   `json:"head"`
	Final   uint64   `json:"final,omitempty"`
}

var (
	ErrInvalidSpec   = errors.New("jobbatch: invalid spec")
	ErrInvalidEvent  = errors.New("jobbatch: invalid, stale or out-of-order event")
	ErrDuplicateRoot = errors.New("jobbatch: duplicate root")
	ErrUnknownNode   = errors.New("jobbatch: unknown node")
	ErrTerminal      = errors.New("jobbatch: node is terminal")
	ErrNotTerminal   = errors.New("jobbatch: node is not terminal")
	ErrNotStarted    = errors.New("jobbatch: execution not started")
	ErrUnsupported   = errors.New("jobbatch: unsupported for this node or execution")
	ErrReleased      = errors.New("jobbatch: root released")
	ErrClosing       = errors.New("jobbatch: runner closing")
	ErrCommitFailed  = errors.New("jobbatch: commit failed")
	ErrClosed        = errors.New("jobbatch: runner closed")
	ErrCloseTimeout  = errors.New("jobbatch: close timed out before final")
	ErrGap           = errors.New("jobbatch: feed gap")
	ErrExecutorPanic = errors.New("jobbatch: executor panic")
	ErrExecutionDone = errors.New("jobbatch: execution already done")
	ErrDispose       = errors.New("jobbatch: dispose")
	ErrCapacity      = errors.New("jobbatch: capacity refused")
)

// DisposeFailure names one handle whose cleanup did not succeed.
type DisposeFailure struct {
	Node  ID
	Token uint64
	Code  string // dispose_failed, dispose_panic, dispose_timeout or dispose_pending.
}

// DisposeError is the error behind ErrDispose. It describes handle cleanup,
// never a task outcome.
type DisposeError struct {
	Failures []DisposeFailure
}

func (e *DisposeError) Error() string {
	var b strings.Builder
	b.WriteString(ErrDispose.Error())
	for i, f := range e.Failures {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s#%d %s", f.Node, f.Token, f.Code)
	}
	return b.String()
}

func (e *DisposeError) Is(target error) bool { return target == ErrDispose }

// CloseTimeoutError is the error behind ErrCloseTimeout; it names the nodes
// that had not reached a terminal state.
type CloseTimeoutError struct {
	Nodes []ID
}

func (e *CloseTimeoutError) Error() string {
	var b strings.Builder
	b.WriteString(ErrCloseTimeout.Error())
	for i, n := range e.Nodes {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString(", ")
		}
		b.WriteString(string(n))
	}
	return b.String()
}

func (e *CloseTimeoutError) Is(target error) bool { return target == ErrCloseTimeout }

// Reserved runtime machine codes. A raw code from an executor that equals one
// of these is replaced by CodeInvalid before it reaches the engine.
const (
	CodeStartFailed          = "start_failed"
	CodeStartNilExecution    = "start_nil_execution"
	CodeStartPanic           = "start_panic"
	CodeExecutorInvalidState = "executor_invalid_state"
	CodeInvalid              = "invalid_code"
	CodeStopFailed           = "stop_failed"
	CodeStopPanic            = "stop_panic"
	CodeStopTimeout          = "stop_timeout"
	CodeDisposeFailed        = "dispose_failed"
	CodeDisposePanic         = "dispose_panic"
	CodeDisposeTimeout       = "dispose_timeout"
	CodeDisposePending       = "dispose_pending"
)

const (
	// MaxCodeBytes is the longest machine code.
	MaxCodeBytes = 64
	// MaxRefBytes is the longest caller reference.
	MaxRefBytes = 128
)

// ValidCode reports whether code is empty or 1-MaxCodeBytes bytes of
// [a-z0-9_.-].
func ValidCode(code string) bool {
	if len(code) > MaxCodeBytes {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

// ReservedCode reports whether code is one of the reserved runtime codes.
func ReservedCode(code string) bool {
	switch code {
	case CodeStartFailed, CodeStartNilExecution, CodeStartPanic,
		CodeExecutorInvalidState, CodeInvalid, CodeStopFailed, CodeStopPanic,
		CodeStopTimeout, CodeDisposeFailed, CodeDisposePanic, CodeDisposeTimeout,
		CodeDisposePending:
		return true
	}
	return false
}

// ValidRef reports whether ref is empty or 1-MaxRefBytes bytes of
// [A-Za-z0-9._:-].
func ValidRef(ref string) bool {
	if len(ref) > MaxRefBytes {
		return false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '.', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// EventType names an engine input. Its string equals the record type the
// accepted event produces.
type EventType string

const (
	EventSubmit            EventType = "submit"
	EventAdd               EventType = "add"
	EventStopRequested     EventType = "stop_requested"
	EventStarted           EventType = "started"
	EventStartRejected     EventType = "start_rejected"
	EventSettled           EventType = "settled"
	EventControlReported   EventType = "control"
	EventCapacityAvailable EventType = "capacity_available"
	EventDeadlineExpired   EventType = "deadline_expired"
	EventHostClosing       EventType = "host_closing"
	EventRelease           EventType = "release"
)

// RejectReason says why a start attempt did not produce an execution.
type RejectReason string

const (
	RejectCapacity  RejectReason = "capacity"
	RejectFailure   RejectReason = "failure"
	RejectCancelled RejectReason = "cancelled"
)

// ControlOp is the control operation a ControlReported answers.
type ControlOp string

const ControlOpStop ControlOp = "stop"

// Event is one engine input. Fields not listed for Type must be zero; the
// engine rejects an event that sets them. Direct Engine callers transfer
// ownership of the event to Apply.
//
//	submit             Root, Spec, Ref
//	add                Parent, Items, Ref
//	stop_requested     Node
//	started            Node, Token
//	start_rejected     Node, Token, Reason, Code
//	settled            Node, Token, Result
//	control            Node, Token, Op, Code
//	capacity_available Group (the empty string is the default group)
//	deadline_expired   Node, Token
//	host_closing       none
//	release            Root
type Event struct {
	Type   EventType    `json:"type"`
	Root   ID           `json:"root,omitempty"`
	Parent ID           `json:"parent,omitempty"`
	Node   ID           `json:"node,omitempty"`
	Spec   Spec         `json:"spec"`
	Items  []Spec       `json:"items,omitempty"`
	Ref    string       `json:"ref,omitempty"`
	Token  uint64       `json:"token,omitempty"`
	Reason RejectReason `json:"reason,omitempty"`
	Op     ControlOp    `json:"op,omitempty"`
	Code   string       `json:"code,omitempty"`
	Result Result       `json:"result"`
	Group  string       `json:"group,omitempty"`
}

// CommandType names an execution command emitted by the engine.
type CommandType string

const (
	CommandStartLeaf CommandType = "start"
	CommandStopLeaf  CommandType = "stop"
)

// Command is an effect the Runtime performs; the engine never executes it.
type Command struct {
	Type  CommandType `json:"type"`
	Node  ID          `json:"node"`
	Token uint64      `json:"token"`
	Cause Cause       `json:"cause,omitempty"` // StopLeaf only.

	// Launch is the full immutable launch of a StartLeaf; zero for StopLeaf.
	Launch Launch `json:"launch"`
}

// Notice is metadata of one selected notification; never a payload.
type Notice struct {
	Root   ID       `json:"root"`
	Node   ID       `json:"node"`
	Kind   NodeKind `json:"kind"`
	State  State    `json:"state"`
	Cause  Cause    `json:"cause,omitempty"`
	Counts Counts   `json:"counts"`
}

// Transition is the result of one accepted event.
type Transition struct {
	// Sequence is the monotonic count of accepted events, this one included.
	Sequence uint64
	Event    Event
	Changed  []NodeView // Changed nodes only, in settlement/creation order.
	Commands []Command
	Notices  []Notice
	// SettledRoots lists the roots that became terminal in this transition,
	// each with the aggregate Batch.Wait will return.
	SettledRoots []Summary
	// Final is true exactly once: HostClosing has been applied and every root
	// is terminal after this transition.
	Final bool
}

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
