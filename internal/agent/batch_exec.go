package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/google/uuid"
)

// Batch leaf kinds. R11 owns the tool-name -> kind -> group -> rank mapping;
// the executor only routes by kind.
const (
	batchKindGeneric = "generic" // any tool run to completion through its policy leaf
	batchKindMCP     = "mcp"     // an MCP tool through its policy leaf
	batchKindShell   = "shell"   // bash with a retained background-shell handle
	batchKindArgv    = "argv"    // run_command with a retained process handle
	batchKindAgent   = "agent"   // delegation scope (agent, agentic_fetch)
)

// batchLeafPayload is the JSON carried in jobbatch.TaskSpec.Payload for every
// kind. Input is the model's tool input exactly as the policy leaf expects it.
// RegistryID and Policy are the R11C ingress transport: when set, leaf
// resolution is keyed by the invocation's pinned registry identity, never by
// session, and Policy carries the invocation's immutable executor context.
type batchLeafPayload struct {
	Tool            string                  `json:"tool"`
	ToolCallID      string                  `json:"tool_call_id"`
	SessionID       string                  `json:"session_id"`
	Input           json.RawMessage         `json:"input"`
	RegistryID      string                  `json:"registry_id,omitempty"`
	Policy          *batchLeafPolicyContext `json:"policy,omitempty"`
	CallCredentials bool                    `json:"call_credentials,omitempty"`
	// Timeout carries a normalized direct/explicit-leaf wake_only timeout
	// intent: the originally requested seconds. The ABSOLUTE call-start due
	// time is deliberately NOT carried: a now()-derived field would make
	// the canonical spec hash (batch_invocation.go batchInvocationHash,
	// mirroring internal/session/batch_commit.go batchCommitSpecHash)
	// nondeterministic across retries of one logical call, breaking
	// in-flight singleflight matching and live-root attach. The durable
	// call-start anchor is batch_nodes.created_at (set once at reservation,
	// never updated); the session-package check-in scanner
	// (batch_timeout_checkin.go) computes due = created_at + seconds. The
	// scanner reads this JSON shape from batch_nodes.task_payload; the json
	// tags are a cross-package contract, do not rename them.
	Timeout *batchLeafTimeoutSpec `json:"timeout,omitempty"`
	// diskProvider is the in-process resolution of Policy.DiskBindingID,
	// stamped by the executor router before family start; never serialized.
	diskProvider tools.DiskProvider `json:"-"`
}

// batchLeafTimeoutSpec is the durable payload form of a wake_only intent.
type batchLeafTimeoutSpec struct {
	Kind    string `json:"kind"`    // always "wake_only" today
	Seconds int    `json:"seconds"` // originally requested duration
}

type batchLeafEnv struct {
	OutputCache *batchOutputCache
	// Leaves resolves a pinned policy-leaf registry by identity.
	Leaves       func(sessionID string) (*policyLeafRegistry, error)
	DiskBindings *DiskProviderRegistry
	// Ingress lazily resolves the App-owned BatchWorkIngress (R12-H2 P1-4):
	// the agent family passes it onto the run request so the driver resolves
	// pinned leaves through the invocation's authority, never the process
	// global. It is a function because the ingress is bound to the runtime
	// only after install (BindRuntimeIngress); an eager value copy would
	// capture nil forever.
	Ingress func() BatchWorkIngress
}

// batchLeafFamily executes one kind. start follows jobbatch.Executor.Start:
// a non-nil error means no execution exists; report is called exactly once
// after the execution has really stopped.
type batchLeafFamily interface {
	start(ctx context.Context, env batchLeafEnv, l jobbatch.Launch, p batchLeafPayload, report func(jobbatch.Result)) (jobbatch.Execution, error)
}

// Machine codes of start failures produced by the router (valid, nonreserved).
const (
	batchCodeBadPayload  = "bad_payload"
	batchCodeUnknownKind = "unknown_kind"
	batchCodeNoLeaf      = "tool_unavailable"
	// batchCodeNoBinding marks a required process-bound disk binding that
	// is no longer available (batchLeafPayload.Policy.DiskBindingID).
	batchCodeNoBinding = "binding_unavailable"
)

// batchStartError carries a safe machine code for the Runner's code rule.
type batchStartError struct {
	code string
	err  error
}

func (e *batchStartError) Error() string { return e.code + ": " + e.err.Error() }
func (e *batchStartError) Code() string  { return e.code }
func (e *batchStartError) Unwrap() error { return e.err }

// batchExecutor is the Rush jobbatch.Executor: it routes a launch to the family
// registered for its kind. It is inert until R11 wires it to a Runner.
type batchExecutor struct {
	env               batchLeafEnv
	families          map[string]batchLeafFamily
	decorateExecution func(jobbatch.Execution) jobbatch.Execution
}

var _ jobbatch.Executor = (*batchExecutor)(nil)

func newBatchExecutor(env batchLeafEnv, families map[string]batchLeafFamily) *batchExecutor {
	return &batchExecutor{env: env, families: families}
}

func (e *batchExecutor) Start(ctx context.Context, l jobbatch.Launch, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	family, ok := e.families[l.Task.Kind]
	if !ok {
		return nil, &batchStartError{batchCodeUnknownKind, errors.New("no family for kind")}
	}
	var p batchLeafPayload
	if err := json.Unmarshal(l.Task.Payload, &p); err != nil || p.Tool == "" || p.SessionID == "" {
		return nil, &batchStartError{batchCodeBadPayload, errors.New("payload needs tool, session_id and input")}
	}
	// A required provider dependency is never accepted with only the
	// legacy boolean/free-text marker; this avoids OS-disk promotion.
	if p.Policy != nil && (p.Policy.DiskProvider || strings.TrimSpace(p.Policy.CustomDisk) != "") && p.Policy.DiskBindingID == "" {
		return nil, &batchStartError{batchCodeNoBinding, ErrDiskProviderUnavailable}
	}
	// Execution owner (R12-H2 §1.2): acquired BEFORE resolving leaves or
	// starting any side effect. One key per accepted execution — instance
	// is a fresh unique id, the key names the construction the execution
	// needs and retains the binding. It is released ONLY by the
	// interface-preserving physical-Dispose wrapper (batch_exec_binding.go),
	// after provider operations/readers and the child-driver exit drain;
	// never on Stop acknowledgement. A disposal timeout/error retains the
	// owner, so start-error paths are the only early releasers here.
	var execRelease func()
	if p.RegistryID != "" {
		if e.env.DiskBindings == nil {
			return nil, &batchStartError{batchCodeNoBinding, ErrDiskProviderUnavailable}
		}
		bindingID := ""
		if p.Policy != nil {
			bindingID = p.Policy.DiskBindingID
			if bindingID != "" {
				if _, err := e.env.DiskBindings.Lookup(bindingID); err != nil {
					return nil, &batchStartError{batchCodeNoBinding, err}
				}
			}
		}
		execKey := diskOwnerKey{kind: diskOwnerExecution, instance: uuid.NewString(), bindingID: bindingID, registryID: p.RegistryID}
		if err := e.env.DiskBindings.acquire(execKey); err != nil {
			return nil, &batchStartError{batchCodeNoBinding, err}
		}
		var once sync.Once
		execRelease = func() { once.Do(func() { e.env.DiskBindings.release(execKey) }) }
		if bindingID != "" {
			registry, err := e.env.Leaves(p.RegistryID)
			if err != nil || registry == nil || !registry.hasToolset(p.Policy.RestrictedToolset) {
				if err == nil {
					err = ErrDiskProviderUnavailable
				}
				execRelease()
				return nil, &batchStartError{batchCodeNoBinding, err}
			}
			leaf, err := registry.resolve(p.Tool)
			if err != nil {
				execRelease()
				return nil, &batchStartError{batchCodeNoBinding, err}
			}
			_ = leaf
			provider, err := e.env.DiskBindings.Lookup(bindingID)
			if err != nil {
				execRelease()
				return nil, &batchStartError{batchCodeNoBinding, err}
			}
			p.diskProvider = provider
		}
	}
	execution, err := family.start(ctx, e.env, l, p, report)
	if err != nil {
		if execRelease != nil {
			execRelease()
		}
		return nil, err
	}
	if execution == nil {
		if execRelease != nil {
			execRelease()
		}
		return nil, &batchStartError{batchCodeNoBinding, ErrDiskProviderUnavailable}
	}
	if e.decorateExecution != nil {
		execution = e.decorateExecution(execution)
	}
	if execRelease != nil {
		return newBatchBindingExecution(execution, execRelease), nil
	}
	return execution, nil
}

// batchLeafLookupID returns the identity leaf resolution is keyed under: the
// invocation's pinned registry identity when present, else the legacy
// session-keyed lookup.
func batchLeafLookupID(p batchLeafPayload) string {
	if p.RegistryID != "" {
		return p.RegistryID
	}
	return p.SessionID
}

// resolveLeaf returns the pinned policy leaf for the payload's tool.
func (env batchLeafEnv) resolveLeaf(p batchLeafPayload) (fantasy.AgentTool, error) {
	if env.Leaves == nil {
		return nil, &batchStartError{batchCodeNoLeaf, errors.New("no toolset provider")}
	}
	registry, err := env.Leaves(batchLeafLookupID(p))
	if err != nil {
		return nil, &batchStartError{batchCodeNoLeaf, err}
	}
	tool, err := registry.resolve(p.Tool)
	if err != nil {
		return nil, &batchStartError{batchCodeNoLeaf, err}
	}
	return tool, nil
}

// batchEnvelope marshals a tool response as the full result envelope carried in
// jobbatch.Result.Details (text, media, metadata and the error flag).
func batchEnvelope(resp fantasy.ToolResponse) json.RawMessage {
	data, err := json.Marshal(resp)
	if err != nil {
		return nil
	}
	return data
}

// batchResultFromResponse maps a finished leaf Run to a jobbatch.Result. A
// denial or failure never becomes success: an error response or a Run error
// settles Failed. cancelled says the execution was stopped by a Stop request.
// A Run error with an empty response gets an error envelope so the stored
// result cannot read as a successful empty one; a response that carries
// content or media (partial output of a stopped or failed leaf) is kept.
func batchResultFromResponse(resp fantasy.ToolResponse, err error, cancelled bool) jobbatch.Result {
	if err != nil && resp.Content == "" && len(resp.Data) == 0 {
		metadata := resp.Metadata
		resp = fantasy.NewTextErrorResponse(err.Error())
		resp.Metadata = metadata
	}
	r := jobbatch.Result{Details: batchEnvelope(resp)}
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
