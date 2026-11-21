package batchcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// DirectCallPreparation carries the root-owned invocation and wire-format
// adapters. Validate may normalize the invocation before its fields are read.
type DirectCallPreparation struct {
	Validate         func() error
	ControlTarget    func(string) bool
	CanonicalInput   func(string) (json.RawMessage, error)
	BuildSpec        func(json.RawMessage) (jobbatch.Spec, error)
	StampCredentials func(context.Context, jobbatch.Spec) (jobbatch.Spec, error)
	AgentDepth       *int
	AgentDepthSet    *bool
	DefaultDepth     int
	MaxDepth         int
	MissingCallID    error
	NilLeaf          error
	ControlPlaneLeaf error
}

// PrepareDirectCall validates and prepares a direct call before any runtime
// wiring check or attach probe. A zero spec means return the response/error
// immediately, including model-input errors that do not end the turn.
func PrepareDirectCall(ctx context.Context, leaf fantasy.AgentTool, call fantasy.ToolCall, p DirectCallPreparation) (jobbatch.Spec, fantasy.ToolResponse, error) {
	handoff := toolwrap.HandoffFrom(ctx)
	release := func() {
		if handoff != nil {
			handoff.Release()
		}
	}
	if err := p.Validate(); err != nil {
		release()
		return jobbatch.Spec{}, fantasy.ToolResponse{}, err
	}
	if call.ID == "" {
		release()
		return jobbatch.Spec{}, fantasy.ToolResponse{}, fmt.Errorf("%w", p.MissingCallID)
	}
	if leaf == nil {
		release()
		return jobbatch.Spec{}, fantasy.ToolResponse{}, fmt.Errorf("%w: %q", p.NilLeaf, call.Name)
	}
	// D1 controls execute their pinned policy leaves, never generic batches.
	if p.ControlTarget(call.Name) {
		release()
		return jobbatch.Spec{}, fantasy.ToolResponse{}, fmt.Errorf("%w: %q", p.ControlPlaneLeaf, call.Name)
	}
	if err := ctx.Err(); err != nil {
		release()
		return jobbatch.Spec{}, fantasy.ToolResponse{}, err
	}
	if directAgentFamily(call.Name) {
		if !*p.AgentDepthSet {
			*p.AgentDepth = p.DefaultDepth
			*p.AgentDepthSet = true
		}
		if *p.AgentDepth < 0 || *p.AgentDepth > p.MaxDepth {
			release()
			return jobbatch.Spec{}, fantasy.ToolResponse{}, errors.New("agent depth must be between 0 and 32")
		}
		if opts := agentcall.CallOptionsFrom(ctx); opts != nil && opts.AgentDepthSet && opts.MaxAgentDepth < *p.AgentDepth {
			*p.AgentDepth = opts.MaxAgentDepth
		}
	}
	input, err := p.CanonicalInput(call.Input)
	if err != nil {
		release()
		return jobbatch.Spec{}, fantasy.NewTextErrorResponse(err.Error()), nil
	}
	spec, err := p.BuildSpec(input)
	if err != nil {
		release()
		var inputErr ToolInputError
		if errors.As(err, &inputErr) {
			return jobbatch.Spec{}, fantasy.NewTextErrorResponse(err.Error()), nil
		}
		return jobbatch.Spec{}, fantasy.ToolResponse{}, err
	}
	spec, err = p.StampCredentials(ctx, spec)
	if err != nil {
		if h := toolwrap.HandoffFrom(ctx); h != nil {
			h.ReleasePending()
		}
		return jobbatch.Spec{}, fantasy.ToolResponse{}, err
	}
	return spec, fantasy.ToolResponse{}, nil
}

// ToolInputError marks a direct call with invalid model input.
type ToolInputError struct{ err error }

func (e ToolInputError) Error() string { return e.err.Error() }
func (e ToolInputError) Unwrap() error { return e.err }

func directAgentFamily(tool string) bool { return tool == "agent" || tool == "agentic_fetch" }

// DirectLeafFields are the derived fields of the root-owned durable payload.
type DirectLeafFields struct {
	SelectedRoleContract *session.RoleContract
	AgentDepth           *int
	WakeOnlySeconds      *int
}

// OneItemSpec builds a direct task, keeping terminate-and-wake in the engine
// deadline and wake-only seconds in the durable payload (never a wall clock).
func OneItemSpec(call fantasy.ToolCall, input json.RawMessage, depth int,
	resolveRole func(string) (*session.RoleContract, error),
	encodePayload func(DirectLeafFields) (json.RawMessage, error),
	kindOverride func(string) (string, bool),
) (jobbatch.Spec, error) {
	var fields DirectLeafFields
	if call.Name == "agent" {
		var selected struct {
			Role agentcall.RequestedModelRole `json:"role"`
		}
		if err := json.Unmarshal(input, &selected); err != nil {
			return jobbatch.Spec{}, ToolInputError{fmt.Errorf("invalid agent role input: %w", err)}
		}
		if selected.Role != "" && !selected.Role.Builtin() {
			var err error
			fields.SelectedRoleContract, err = resolveRole(string(selected.Role))
			if err != nil {
				return jobbatch.Spec{}, err
			}
		}
	}
	intent, err := batchinst.ParseTimeoutParam(call.Name, string(input))
	if err != nil {
		return jobbatch.Spec{}, ToolInputError{err}
	}
	if directAgentFamily(call.Name) {
		fields.AgentDepth = &depth
	}
	var timeout time.Duration
	if intent != nil {
		if intent.Kind == batchinst.TimeoutTerminateAndWake {
			timeout = intent.Duration
		} else {
			fields.WakeOnlySeconds = &intent.Seconds
		}
	}
	raw, err := encodePayload(fields)
	if err != nil {
		return jobbatch.Spec{}, fmt.Errorf("batch payload: %w", err)
	}
	return jobbatch.Spec{Task: &jobbatch.TaskSpec{
		Kind: LeafKind(call.Name, kindOverride), Payload: raw, Timeout: timeout,
	}}, nil
}

// LeafKind routes built-in families first, then explicitly registered MCP leaves.
func LeafKind(tool string, override func(string) (string, bool)) string {
	switch tool {
	case "bash":
		return batchdispatch.KindShell
	case "run_command":
		return batchdispatch.KindArgv
	case "agent", "agentic_fetch":
		return batchdispatch.KindAgent
	}
	if kind, ok := override(tool); ok {
		return kind
	}
	return batchdispatch.KindGeneric
}

// FreshCall binds the invocation-specific ports used after singleflight election.
type FreshCall struct {
	Attach           AttachStore
	MatchLease       func(jobbatch.ID) bool // nil when attaching does not require a lease check
	LeaseUnavailable error
	ReleaseHandoff   func(context.Context)
	Submit           func(context.Context) (jobbatch.ID, error)
	Deliver          func(context.Context, jobbatch.ID) (fantasy.ToolResponse, error)
}

// Invoke runs a prepared call, retrying if a previous caller poisoned the shared outcome.
func (r *Ingress) Invoke(ctx context.Context, probe AttachProbe, fresh FreshCall, mismatch error) (fantasy.ToolResponse, error) {
	// Singleflight covers only IN-FLIGHT executions: finishCall removes the
	// entry before publishing, so a retry arriving after completion takes
	// the fresh durable path (design I5: a reused call id after a history
	// rerun is a new execution, never a cached replay). A waiter whose own
	// context is still alive but that received the first caller's
	// cancellation re-runs the fresh path as the new first caller; the loop
	// bound only guards against a pathological store that keeps returning
	// foreign cancellation errors.
	for attempts := 0; ; attempts++ {
		resp, err, retry := r.invokeOnce(ctx, probe, fresh, mismatch)
		if !retry || attempts >= 4 {
			return resp, err
		}
	}
}

// invokeOnce runs one singleflight step for a prepared invocation: join an
// in-flight execution or become its first caller.
func (r *Ingress) invokeOnce(ctx context.Context, probe AttachProbe, fresh FreshCall, mismatch error) (fantasy.ToolResponse, error, bool) {
	r.mu.Lock()
	key := probe.Owner + "\x00" + probe.ToolCallID
	if e, ok := r.calls[key]; ok {
		r.mu.Unlock()
		if e.hash != probe.InputHash {
			return fantasy.ToolResponse{}, fmt.Errorf("%w: call %q", mismatch, probe.ToolCallID), false
		}
		select {
		case e.joined <- struct{}{}:
		default:
		}
		resp, err := waitCall(ctx, e)
		// Waiter poisoning: the published outcome is the FIRST caller's
		// cancellation, not this waiter's (its own ctx is still alive) and
		// the entry is already removed, so re-run the fresh path.
		if ctx.Err() == nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return fantasy.ToolResponse{}, nil, true
		}
		return resp, err, false
	}
	e := &batchCallEntry{hash: probe.InputHash, done: make(chan struct{}), joined: make(chan struct{}, batchCallWaiterBuffer)}
	r.calls[key] = e
	r.mu.Unlock()

	resp, err := fresh.run(ctx, probe)
	r.finishCall(key, e, resp, err)
	return resp, err, false
}

// run executes the first execution of one call identity: durable attach
// probe, admission, Submit, delivery.
func (f FreshCall) run(ctx context.Context, probe AttachProbe) (fantasy.ToolResponse, error) {
	root, attached, err := f.Attach.ProbeLive(ctx, probe)
	if err != nil {
		f.ReleaseHandoff(ctx)
		return fantasy.ToolResponse{}, fmt.Errorf("batch attach: %w", err)
	}
	if attached && f.MatchLease != nil && !f.MatchLease(root) {
		f.ReleaseHandoff(ctx)
		return fantasy.ToolResponse{}, f.LeaseUnavailable
	}
	if !attached {
		root, err = f.Submit(ctx)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
	} else {
		f.ReleaseHandoff(ctx)
	}
	resp, err := f.Deliver(ctx, root)
	// Accepted physical work cannot be preempted by caller cancellation
	// (R12-H2 §1.2: physical I/O Stop cannot preempt). The first caller's
	// cancellation Stops the root once (deliverSync) and returns its ctx
	// error, but the call's owner scope must NOT unwind beneath the still
	// executing root: re-wait WITHOUT the caller's cancellation for the
	// root's real completion, so the call/construction owners are released
	// only after the physical settle — never by a waiter leaving early.
	if err != nil && ctx.Err() != nil &&
		(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		resp, err = f.Deliver(context.WithoutCancel(ctx), root)
	}
	// Settled roots stay RETAINED by the engine by design (design §1.2): no
	// per-call release happens here — execution owners release only at
	// physical Dispose (Runner.Release/cleanup).
	return resp, err
}

// SubmitParams carries the prepared invocation and the ports for starting a fresh root.
type SubmitParams struct {
	Owner, RegistryID, DiskBindingID, CallID string
	Spec                                     jobbatch.Spec
	Runner                                   interface {
		Submit(context.Context, jobbatch.SubmitRequest) (jobbatch.ID, error)
	}
	Admit            func(jobbatch.ID, jobbatch.Spec) error
	InvalidAdmission error
	LeaseUnavailable error
	HasDiskBindings  bool
	HandoffFrom      func(context.Context) *toolwrap.PctDelegationHandoff
	// AcquireRoot is nil when no root lease store is wired.
	AcquireRoot func(root jobbatch.ID, owner, binding, registry string) error
	ReleaseRoot func(root jobbatch.ID, binding, registry string)
	AdoptRoot   func(root jobbatch.ID, done <-chan struct{}, binding, registry string, release func(context.Context, jobbatch.ID) error) error
}

type boundSubmitRunner interface {
	SubmitBound(context.Context, jobbatch.SubmitRequest) (jobbatch.ID, *jobbatch.Batch, error)
}

type submitReleaseRunner interface {
	Release(context.Context, jobbatch.ID) error
}

// Submit admits and starts a fresh one-item root, transferring ownership only
// after the runner returns the requested root.
func Submit(ctx context.Context, p SubmitParams) (jobbatch.ID, error) {
	root := RootID()
	if err := p.Admit(root, p.Spec); err != nil {
		if handoff := p.HandoffFrom(ctx); handoff != nil {
			handoff.Release()
		}
		return "", fmt.Errorf("%w: %w", p.InvalidAdmission, err)
	}
	// Root owner: acquired BEFORE Submit with the already-minted root ID;
	// it covers accepted-but-not-started children. Released on definite
	// rejection below; a successful Submit keeps it and the Done watcher
	// releases it when no further child can start.
	rootOwned := false
	pctHandoff := p.HandoffFrom(ctx)
	if pctHandoff != nil {
		if !p.HasDiskBindings || pctHandoff.HasReleases() == 0 {
			pctHandoff.Release()
			return "", p.LeaseUnavailable
		}
	}
	if p.RegistryID != "" {
		if p.AcquireRoot == nil {
			if pctHandoff != nil {
				pctHandoff.Release()
			}
			return "", p.LeaseUnavailable
		}
		if err := p.AcquireRoot(root, p.Owner, p.DiskBindingID, p.RegistryID); err != nil {
			if pctHandoff != nil {
				pctHandoff.Release()
			}
			return "", err
		}
		rootOwned = true
	}
	releaseRoot := func() {
		if rootOwned {
			p.ReleaseRoot(root, p.DiskBindingID, p.RegistryID)
		}
	}
	request := jobbatch.SubmitRequest{Root: root, Spec: p.Spec, Ref: p.CallID}
	var id jobbatch.ID
	var batch *jobbatch.Batch
	var err error
	if rootOwned {
		bound, ok := p.Runner.(boundSubmitRunner)
		if !ok {
			releaseRoot()
			if pctHandoff != nil {
				pctHandoff.Release()
			}
			return "", p.LeaseUnavailable
		}
		id, batch, err = bound.SubmitBound(ctx, request)
	} else {
		id, err = p.Runner.Submit(ctx, request)
	}
	if err != nil {
		releaseRoot()
		if pctHandoff != nil {
			pctHandoff.Release()
		}
		return "", fmt.Errorf("batch submit: %w", err)
	}
	if id != root {
		releaseRoot()
		if pctHandoff != nil {
			pctHandoff.Release()
		}
		return "", fmt.Errorf("batch submit: runner root %q differs from requested %q", id, root)
	}
	if rootOwned {
		if batch == nil {
			releaseRoot()
			if pctHandoff != nil {
				pctHandoff.Release()
			}
			return "", p.LeaseUnavailable
		}
		if batch.ID() != root {
			releaseRoot()
			if pctHandoff != nil {
				pctHandoff.Release()
			}
			return "", fmt.Errorf("batch submit: runner root %q differs from requested %q", batch.ID(), root)
		}
		release, ok := p.Runner.(submitReleaseRunner)
		if !ok {
			if pctHandoff != nil {
				pctHandoff.Release()
			}
			return "", p.LeaseUnavailable
		}
		if err := p.AdoptRoot(root, batch.Done(), p.DiskBindingID, p.RegistryID, release.Release); err != nil {
			releaseRoot()
			if pctHandoff != nil {
				pctHandoff.Release()
			}
			return "", err
		}
	}
	if pctHandoff != nil {
		// pct: successful Submit transfers cleanup to the accepted-root Done watcher.
		pctHandoff.Accepted()
		if batch != nil {
			go func() {
				<-batch.Done()
				pctHandoff.ReleaseUnstarted()
			}()
		}
	}
	return root, nil
}
