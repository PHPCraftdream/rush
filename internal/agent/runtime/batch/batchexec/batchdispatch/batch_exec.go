package batchdispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// StartError carries a safe machine code for the Runner's code rule.
type StartError struct {
	code string
	err  error
}

// NewStartError pairs a machine code with the underlying start failure.
func NewStartError(code string, err error) *StartError {
	return &StartError{code: code, err: err}
}

func (e *StartError) Error() string { return e.code + ": " + e.err.Error() }
func (e *StartError) Code() string  { return e.code }
func (e *StartError) Unwrap() error { return e.err }

// Executor dispatches a launch through typed host adapters.
type Executor[E, P any] struct {
	env                E
	Family             func(string) (LeafFamily[E, P], bool)
	Prepare            func(context.Context, E, json.RawMessage) (P, func(), error)
	DecorateExecution  func(jobbatch.Execution) jobbatch.Execution
	WithLease          func(jobbatch.Execution, func()) jobbatch.Execution
	StartError         func(string, error) error
	BindingUnavailable error
}

// LeafFamily starts one kind; a start error means no execution was accepted.
type LeafFamily[E, P any] func(context.Context, E, jobbatch.Launch, P, func(jobbatch.Result)) (jobbatch.Execution, error)

// NewExecutor binds the host environment and adapters used to dispatch launches.
func NewExecutor[E, P any](
	env E,
	family func(string) (LeafFamily[E, P], bool),
	prepare func(context.Context, E, json.RawMessage) (P, func(), error),
	withLease func(jobbatch.Execution, func()) jobbatch.Execution,
	startError func(string, error) error,
	bindingUnavailable error,
) Executor[E, P] {
	return Executor[E, P]{
		env:                env,
		Family:             family,
		Prepare:            prepare,
		WithLease:          withLease,
		StartError:         startError,
		BindingUnavailable: bindingUnavailable,
	}
}

var _ jobbatch.Executor = Executor[struct{}, struct{}]{}

func (e Executor[E, P]) Start(ctx context.Context, l jobbatch.Launch, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	family, ok := e.Family(l.Task.Kind)
	if !ok {
		return nil, e.StartError(CodeUnknownKind, errors.New("no family for kind"))
	}
	payload, release, err := e.Prepare(ctx, e.env, l.Task.Payload)
	if err != nil {
		return nil, err
	}
	execution, err := family(ctx, e.env, l, payload, report)
	if err != nil || execution == nil {
		if release != nil {
			release()
		}
		if err != nil {
			return nil, err
		}
		return nil, e.StartError(CodeNoBinding, e.BindingUnavailable)
	}
	if e.DecorateExecution != nil {
		execution = e.DecorateExecution(execution)
	}
	if release != nil {
		execution = e.WithLease(execution, release)
	}
	return execution, nil
}

func LeafLookupID(registryID, sessionID string) string {
	if registryID != "" {
		return registryID
	}
	return sessionID
}

// Batch leaf kinds. R11 owns the tool-name -> kind -> group -> rank mapping;
// the executor only routes by kind.
const (
	KindGeneric = "generic" // any tool run to completion through its policy leaf
	KindMCP     = "mcp"     // an MCP tool through its policy leaf
	KindShell   = "shell"   // bash with a retained background-shell handle
	KindArgv    = "argv"    // run_command with a retained process handle
	KindAgent   = "agent"   // delegation scope (agent, agentic_fetch)
)

// Machine codes of start failures produced by the router (valid, nonreserved).
const (
	CodeBadPayload  = "bad_payload"
	CodeUnknownKind = "unknown_kind"
	CodeNoLeaf      = "tool_unavailable"
	// CodeNoBinding marks a required process-bound disk binding that
	// is no longer available (batchLeafPayload.Policy.DiskBindingID).
	CodeNoBinding = "binding_unavailable"
)

// Envelope marshals a tool response as the full result envelope carried in
// jobbatch.Result.Details (text, media, metadata and the error flag).
func Envelope(resp fantasy.ToolResponse) json.RawMessage {
	data, err := json.Marshal(resp)
	if err != nil {
		return nil
	}
	return data
}

// ResultFromResponse maps a finished leaf Run to a jobbatch.Result. A
// denial or failure never becomes success: an error response or a Run error
// settles Failed. cancelled says the execution was stopped by a Stop request.
// A Run error with an empty response gets an error envelope so the stored
// result cannot read as a successful empty one; a response that carries
// content or media (partial output of a stopped or failed leaf) is kept.
func ResultFromResponse(resp fantasy.ToolResponse, err error, cancelled bool) jobbatch.Result {
	if err != nil && resp.Content == "" && len(resp.Data) == 0 {
		metadata := resp.Metadata
		resp = fantasy.NewTextErrorResponse(err.Error())
		resp.Metadata = metadata
	}
	r := jobbatch.Result{Details: Envelope(resp)}
	switch {
	case cancelled:
		r.State = jobbatch.StateCancelled
	case err != nil:
		r.State, r.Code = jobbatch.StateFailed, "tool_run_error"
		r.Summary = fmt.Sprintf("%v", err)
	case resp.IsError:
		r.State, r.Code = jobbatch.StateFailed, "tool_error"
	default:
		r.State = jobbatch.StateCompleted
	}
	if r.Summary == "" {
		r.Summary = resp.Content
	}
	return r
}
