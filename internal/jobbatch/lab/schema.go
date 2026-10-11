package lab

import (
	"encoding/json"
	"errors"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// Scenario is the version-1 document; ingress wraps only raw root tasks.
// Laboratory owns loader/executors/scripts; Runtime owns OneItem and Runner.
// Slice test helpers use labTest*, schema tests use schemaTest*.
// Process strategy: platform.Command, Unix process groups, Windows suspended
// spawn assigned to a kill-on-close Job Object before resume (design 5.4).
type Scenario struct {
	Version     int                        `json:"version"`
	Limits      jobbatch.Limits            `json:"limits"`
	Runner      RunnerSettings             `json:"runner"`
	WaitBoundMS int64                      `json:"wait_bound_ms"`
	Payloads    map[string]Payload         `json:"payloads"`
	Messages    map[string]json.RawMessage `json:"messages,omitempty"`
	Script      []Action                   `json:"script"`
	Expect      *Expect                    `json:"expect,omitempty"`
}

type RunnerSettings struct {
	ControlTimeoutMS int64 `json:"control_timeout_ms"`
	RecordRetention  int   `json:"record_retention"`
}

// Spec is document-owned: payload references are not engine payloads or refs.
// Conversion maps the referenced payload's Kind to jobbatch.TaskSpec.Kind and
// marshals that payload into TaskSpec.Payload. Group is copied; TimeoutMS is
// multiplied by time.Millisecond for TaskSpec.Timeout. Batch conversion copies
// Mode, OnFail, Notify and MaxParallel and recursively converts Items. Only raw
// root tasks are normalized through OneItem at run/replay ingress; never Add
// items or nested tasks.
type Spec struct {
	Task  *TaskSpec  `json:"task,omitempty"`
	Batch *BatchSpec `json:"batch,omitempty"`
}

type TaskSpec struct {
	PayloadRef string  `json:"payload_ref"`
	Group      *string `json:"group,omitempty"`
	TimeoutMS  *int64  `json:"timeout_ms,omitempty"`
}

type BatchSpec struct {
	Mode        jobbatch.Mode    `json:"mode"`
	OnFail      *jobbatch.OnFail `json:"on_fail,omitempty"`
	Notify      *jobbatch.Notify `json:"notify,omitempty"`
	MaxParallel *int             `json:"max_parallel,omitempty"`
	Items       []Spec           `json:"items"`
}

// Payload is controlled (kind only) or process (direct argv, never shell text).
type Payload struct {
	Kind           string    `json:"kind"`
	Program        *string   `json:"program,omitempty"`
	Args           *[]string `json:"args,omitempty"`
	Cwd            *string   `json:"cwd,omitempty"`
	MaxOutputBytes *int      `json:"max_output_bytes,omitempty"`
}

// ProcessOptions sets a positive retention ceiling; payloads may lower it,
// never raise it. Retention keeps the newest merged-stream bytes.
type ProcessOptions struct {
	MaxRetainedBytes int
}

type StartDecision struct {
	Decision Decision       `json:"decision"`
	State    jobbatch.State `json:"state,omitempty"`
	Code     string         `json:"code,omitempty"` // Raw; Runtime normalizes it.
}

type Decision string

const (
	DecisionAccept         Decision = "accept"
	DecisionAcceptComplete Decision = "accept_complete"
	DecisionRefuseCapacity Decision = "refuse_capacity"
	DecisionFail           Decision = "fail"
)

// GateResult is ok, error:<code>, or panic.
type GateResult string

const (
	GateOK    GateResult = "ok"
	GatePanic GateResult = "panic"
)

type ActionName string

const (
	ActionSubmit            ActionName = "submit"
	ActionAdd               ActionName = "add"
	ActionStop              ActionName = "stop"
	ActionInject            ActionName = "inject"
	ActionCapacityAvailable ActionName = "capacity_available"
	ActionRelease           ActionName = "release"
	ActionClose             ActionName = "close"
	ActionAwaitStart        ActionName = "await_start"
	ActionReleaseStart      ActionName = "release_start"
	ActionComplete          ActionName = "complete"
	ActionEmit              ActionName = "emit"
	ActionAwaitInject       ActionName = "await_inject"
	ActionHoldStop          ActionName = "hold_stop"
	ActionFailStop          ActionName = "fail_stop"
	ActionAwaitStop         ActionName = "await_stop"
	ActionReturnStop        ActionName = "return_stop"
	ActionHoldDispose       ActionName = "hold_dispose"
	ActionAwaitDispose      ActionName = "await_dispose"
	ActionReturnDispose     ActionName = "return_dispose"
	ActionHoldRead          ActionName = "hold_read"
	ActionReturnRead        ActionName = "return_read"
	ActionAwaitState        ActionName = "await_state"
	ActionExpectOutput      ActionName = "expect_output"
	ActionJoin              ActionName = "join"
	ActionAwaitRecord       ActionName = "await_record"
)

type CloseExpectation string

const (
	CloseOK      CloseExpectation = "ok"
	CloseTimeout CloseExpectation = "timeout"
	CloseDispose CloseExpectation = "dispose"
)

// Action is flat; Action selects the field set in design 5.2. Loader validates
// names, phases, conflicting expectations and irrelevant fields; no runtime here.
// Type is the record filter, not the action discriminator. State also admits terminal.
type Action struct {
	Action      ActionName           `json:"action"`
	TimeoutMS   *int64               `json:"timeout_ms,omitempty"`
	Async       *string              `json:"async,omitempty"`
	ExpectError *ErrorName           `json:"expect_error,omitempty"`
	Ref         *string              `json:"ref,omitempty"`
	Root        *jobbatch.ID         `json:"root,omitempty"`
	Parent      *jobbatch.ID         `json:"parent,omitempty"`
	Node        *jobbatch.ID         `json:"node,omitempty"`
	Spec        *Spec                `json:"spec,omitempty"`
	Items       *[]Spec              `json:"items,omitempty"`
	MessageRef  *string              `json:"message_ref,omitempty"`
	Group       *string              `json:"group,omitempty"`
	Expect      *CloseExpectation    `json:"expect,omitempty"`
	Bind        *string              `json:"bind,omitempty"`
	Attempt     *string              `json:"attempt,omitempty"`
	Decision    *Decision            `json:"decision,omitempty"`
	State       *jobbatch.State      `json:"state,omitempty"`
	Code        *string              `json:"code,omitempty"` // Raw on release_start/complete.
	Text        *string              `json:"text,omitempty"`
	Call        *uint64              `json:"call,omitempty"`
	Result      *GateResult          `json:"result,omitempty"`
	Contains    *string              `json:"contains,omitempty"`
	EOF         *bool                `json:"eof,omitempty"`
	Name        *string              `json:"name,omitempty"`
	Type        *jobbatch.RecordType `json:"type,omitempty"`
}

// Oracle pointers preserve unchecked versus exactly zero/empty.
type Expect struct {
	Roots     *[]RootExpectation                   `json:"roots,omitempty"`
	Nodes     *[]NodeExpectation                   `json:"nodes,omitempty"`
	Notices   *map[jobbatch.ID][]NoticeExpectation `json:"notices,omitempty"`
	MaxActive *int                                 `json:"max_active,omitempty"`
	Before    *[][2]string                         `json:"before,omitempty"`
	ExitCode  *int                                 `json:"exit_code,omitempty"`
}

type RootExpectation struct {
	ID     jobbatch.ID      `json:"id"`
	State  *jobbatch.State  `json:"state,omitempty"`
	Counts *jobbatch.Counts `json:"counts,omitempty"`
}

type OutcomeExpectation struct {
	State *jobbatch.State `json:"state,omitempty"`
	Code  *string         `json:"code,omitempty"`
}

type NodeExpectation struct {
	ID             jobbatch.ID         `json:"id"`
	Parent         *jobbatch.ID        `json:"parent,omitempty"`
	State          *jobbatch.State     `json:"state,omitempty"`
	Cause          *jobbatch.Cause     `json:"cause,omitempty"`
	Outcome        *OutcomeExpectation `json:"outcome,omitempty"`
	Control        *string             `json:"control,omitempty"` // none, pending, acked, failed(code).
	ControlPending *bool               `json:"control_pending,omitempty"`
	Attempts       *int                `json:"attempts,omitempty"`
	Starts         *int                `json:"starts,omitempty"`
	Accepted       *int                `json:"accepted,omitempty"`
	Stops          *int                `json:"stops,omitempty"`
	Disposes       *int                `json:"disposes,omitempty"`
}

type NoticeExpectation struct {
	Node   jobbatch.ID      `json:"node"`
	State  *jobbatch.State  `json:"state,omitempty"`
	Cause  *jobbatch.Cause  `json:"cause,omitempty"`
	Counts *jobbatch.Counts `json:"counts,omitempty"`
}

const (
	ExitSuccess           = 0
	ExitFailed            = 1
	ExitInvalid           = 2
	ExitUnfinished        = 3
	ExitMismatch          = 4
	ExitIncompleteJournal = 5
)

type ErrorName string

const (
	ErrorInvalidSpec    ErrorName = "invalid_spec"
	ErrorInvalidEvent   ErrorName = "invalid_event"
	ErrorDuplicateRoot  ErrorName = "duplicate_root"
	ErrorUnknownNode    ErrorName = "unknown_node"
	ErrorTerminal       ErrorName = "terminal"
	ErrorNotTerminal    ErrorName = "not_terminal"
	ErrorNotStarted     ErrorName = "not_started"
	ErrorUnsupported    ErrorName = "unsupported"
	ErrorReleased       ErrorName = "released"
	ErrorClosing        ErrorName = "closing"
	ErrorClosed         ErrorName = "closed"
	ErrorCloseTimeout   ErrorName = "close_timeout"
	ErrorGap            ErrorName = "gap"
	ErrorExecutorPanic  ErrorName = "executor_panic"
	ErrorExecutionDone  ErrorName = "execution_done"
	ErrorDispose        ErrorName = "dispose"
	ErrorCapacity       ErrorName = "capacity"
	ErrorUnknownAttempt ErrorName = "unknown_attempt"
	ErrorAttemptPhase   ErrorName = "attempt_phase"
)

// ErrorNameOf uses errors.Is, including wrapped and joined errors. First match
// wins: cleanup timeout/dispose precede the frozen declaration order, then lab
// errors. Nil and unknown errors return empty; never derive names from text.
func ErrorNameOf(err error) ErrorName {
	for _, entry := range []struct {
		err  error
		name ErrorName
	}{
		{jobbatch.ErrCloseTimeout, ErrorCloseTimeout},
		{jobbatch.ErrDispose, ErrorDispose},
		{jobbatch.ErrInvalidSpec, ErrorInvalidSpec},
		{jobbatch.ErrInvalidEvent, ErrorInvalidEvent},
		{jobbatch.ErrDuplicateRoot, ErrorDuplicateRoot},
		{jobbatch.ErrUnknownNode, ErrorUnknownNode},
		{jobbatch.ErrTerminal, ErrorTerminal},
		{jobbatch.ErrNotTerminal, ErrorNotTerminal},
		{jobbatch.ErrNotStarted, ErrorNotStarted},
		{jobbatch.ErrUnsupported, ErrorUnsupported},
		{jobbatch.ErrReleased, ErrorReleased},
		{jobbatch.ErrClosing, ErrorClosing},
		{jobbatch.ErrClosed, ErrorClosed},
		{jobbatch.ErrGap, ErrorGap},
		{jobbatch.ErrExecutorPanic, ErrorExecutorPanic},
		{jobbatch.ErrExecutionDone, ErrorExecutionDone},
		{jobbatch.ErrCapacity, ErrorCapacity},
		{ErrUnknownAttempt, ErrorUnknownAttempt},
		{ErrAttemptPhase, ErrorAttemptPhase},
	} {
		if errors.Is(err, entry.err) {
			return entry.name
		}
	}
	return ""
}

// JSONL framing: one header, public jobbatch.Record lines, one footer. Header
// and footer use type; records keep their public type. No payload or error text.
type JournalHeader struct {
	Type           string          `json:"type"` // header
	Format         string          `json:"format"`
	Version        int             `json:"version"`
	ScenarioSHA256 string          `json:"scenario_sha256"`
	Limits         jobbatch.Limits `json:"limits"`
	Runner         RunnerSettings  `json:"runner"`
}

type JournalFooter struct {
	Type          string         `json:"type"` // footer
	FinalCursor   *uint64        `json:"final_cursor,omitempty"`
	FinalSequence uint64         `json:"final_sequence"`
	LastCursor    uint64         `json:"last_cursor"`
	Capture       JournalCapture `json:"capture"`
	Outcome       JournalOutcome `json:"outcome"`
	Roots         []JournalRoot  `json:"roots"`
	Inputs        []JournalInput `json:"inputs"`
	RequestCounts RequestCounts  `json:"request_counts"`
}

type CaptureStatus string

const (
	CaptureComplete   CaptureStatus = "complete"
	CaptureGap        CaptureStatus = "gap"
	CaptureWriteError CaptureStatus = "write_error"
)

type JournalCapture struct {
	Status       CaptureStatus `json:"status"`
	FirstMissing *uint64       `json:"first_missing,omitempty"`
	ResumedAt    *uint64       `json:"resumed_at,omitempty"`
}

type OutcomeStatus string

const (
	OutcomeClosed     OutcomeStatus = "closed"
	OutcomeUnfinished OutcomeStatus = "unfinished"
)

type JournalOutcome struct {
	Status OutcomeStatus  `json:"status"`
	IDs    *[]jobbatch.ID `json:"ids,omitempty"`
}

type JournalRoot struct {
	ID              jobbatch.ID     `json:"id"`
	State           jobbatch.State  `json:"state"`
	Counts          jobbatch.Counts `json:"counts"`
	SettledSequence *uint64         `json:"settled_sequence,omitempty"`
}

type InputResult string

const (
	InputAccepted  InputResult = "accepted"
	InputRejected  InputResult = "rejected"
	InputAbandoned InputResult = "abandoned"
)

type JournalInput struct {
	Ref    string         `json:"ref"`
	Action ActionName     `json:"action"`
	Result InputResult    `json:"result"`
	IDs    *[]jobbatch.ID `json:"ids,omitempty"`
	Error  *ErrorName     `json:"error,omitempty"`
}

type RequestCounts struct {
	CapacityAvailable int `json:"capacity_available"`
	Release           int `json:"release"`
}
