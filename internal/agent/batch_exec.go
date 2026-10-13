package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
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
	Tool       string                  `json:"tool"`
	ToolCallID string                  `json:"tool_call_id"`
	SessionID  string                  `json:"session_id"`
	Input      json.RawMessage         `json:"input"`
	RegistryID string                  `json:"registry_id,omitempty"`
	Policy     *batchLeafPolicyContext `json:"policy,omitempty"`
}

// batchLeafEnv gives a family its pinned, policy-wrapped targets.
type batchLeafEnv struct {
	// Leaves returns the registry of the session's pinned toolset.
	Leaves func(sessionID string) (*policyLeafRegistry, error)
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
	env      batchLeafEnv
	families map[string]batchLeafFamily
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
	return family.start(ctx, e.env, l, p, report)
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
func batchResultFromResponse(resp fantasy.ToolResponse, err error, cancelled bool) jobbatch.Result {
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
