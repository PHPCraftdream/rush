package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/google/uuid"
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
	// SelectedRoleContract is the trusted admission-time selection for this
	// agent leaf, separate from the inherited parent ceiling in Policy.
	SelectedRoleContract *session.RoleContract `json:"selected_role_contract,omitempty"`
	CallCredentials      bool                  `json:"call_credentials,omitempty"`
	// QuestionRoute records whether THIS incarnation was submitted through
	// a surface with an established held-question answer route (served API
	// intake or a parent-owned orchestrator delegation). It is stamped by
	// trusted intake code after parsing, never accepted from external or
	// model input; absent means NO route (an unattended batch): ask_question
	// is stripped from the incarnation's toolset and any stale or forged ask
	// call is rejected before a hold or a child_question notice can exist.
	QuestionRoute bool `json:"question_route,omitempty"`
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
	// AgentDepth is the trusted parent budget remaining for an agent leaf.
	AgentDepth *int `json:"agent_depth,omitempty"`
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
	Leaves       func(sessionID string) (*toolwrap.PolicyLeafRegistry, error)
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

// batchExecutor adapts the host's post-construction execution decorator to the
// dispatch router. Installation sets the decorator before publishing the executor.
type batchExecutor struct {
	router            batchdispatch.Executor[batchLeafEnv, batchLeafPayload]
	decorateExecution func(jobbatch.Execution) jobbatch.Execution
}

var _ jobbatch.Executor = (*batchExecutor)(nil)

func newBatchExecutor(env batchLeafEnv, families map[string]batchLeafFamily) *batchExecutor {
	router := batchdispatch.NewExecutor(
		env,
		func(kind string) (batchdispatch.LeafFamily[batchLeafEnv, batchLeafPayload], bool) {
			f, ok := families[kind]
			if !ok {
				return nil, false
			}
			return f.start, true
		},
		prepareBatchLeaf,
		batchexec.WithBindingLease,
		func(code string, err error) error { return batchdispatch.NewStartError(code, err) },
		ErrDiskProviderUnavailable,
	)
	return &batchExecutor{router: router}
}

func (e *batchExecutor) Start(ctx context.Context, l jobbatch.Launch, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	router := e.router
	router.DecorateExecution = e.decorateExecution
	return router.Start(ctx, l, report)
}

func prepareBatchLeaf(ctx context.Context, env batchLeafEnv, raw json.RawMessage) (batchLeafPayload, func(), error) {
	var p batchLeafPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.Tool == "" || p.SessionID == "" {
		return p, nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, errors.New("payload needs tool, session_id and input"))
	}
	if p.Tool == modelkit.AgentToolName {
		if err := validateAgentRoleInput(p.Input); err != nil {
			return p, nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, err)
		}
		var requested struct {
			Role RequestedModelRole `json:"role"`
		}
		if err := json.Unmarshal(p.Input, &requested); err != nil {
			return p, nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, err)
		}
		if requested.Role != "" && !requested.Role.Builtin() {
			if p.SelectedRoleContract == nil || p.SelectedRoleContract.Name != string(requested.Role) {
				return p, nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, fmt.Errorf("%w: selected custom role snapshot missing or mismatched", ErrAgentRoleMismatch))
			}
		} else if p.SelectedRoleContract != nil {
			return p, nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, fmt.Errorf("%w: custom snapshot supplied for built-in or omitted role", ErrAgentRoleMismatch))
		}
	} else if p.SelectedRoleContract != nil {
		return p, nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, fmt.Errorf("%w: custom role snapshot on non-agent leaf", ErrAgentRoleMismatch))
	}
	if p.Policy != nil && p.Policy.RoleContract != nil {
		if err := p.Policy.RoleContract.Validate(); err != nil {
			return p, nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, fmt.Errorf("invalid inherited role contract: %w", err))
		}
	}
	if p.SelectedRoleContract != nil {
		if err := p.SelectedRoleContract.Validate(); err != nil {
			return p, nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, fmt.Errorf("invalid admitted role contract: %w", err))
		}
		if p.Tool != modelkit.AgentToolName || p.SelectedRoleContract.Name == "" || !batchinst.RoleContractInherits(p.SelectedRoleContract, func() *session.RoleContract {
			if p.Policy == nil {
				return nil
			}
			return p.Policy.RoleContract
		}()) {
			return p, nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, errors.New("admitted role contract does not match leaf or inherited ceiling"))
		}
	}
	dispatchCtx := ctx
	if p.Policy != nil && p.Policy.RoleContract != nil {
		dispatchCtx = toolwrap.WithRoleToolContract(dispatchCtx, p.Policy.RoleContract)
	}
	if !toolwrap.RoleToolAllowed(dispatchCtx, p.Tool) {
		return p, nil, batchdispatch.NewStartError(batchdispatch.CodeNoLeaf, toolwrap.GuardRoleContractError(p.Tool))
	}
	// A required provider dependency is never accepted with only the
	// legacy boolean/free-text marker; this avoids OS-disk promotion.
	if p.Policy != nil && (p.Policy.DiskProvider || strings.TrimSpace(p.Policy.CustomDisk) != "") && p.Policy.DiskBindingID == "" {
		return p, nil, batchdispatch.NewStartError(batchdispatch.CodeNoBinding, ErrDiskProviderUnavailable)
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
		if env.DiskBindings == nil {
			return p, nil, batchdispatch.NewStartError(batchdispatch.CodeNoBinding, ErrDiskProviderUnavailable)
		}
		bindingID := ""
		if p.Policy != nil {
			bindingID = p.Policy.DiskBindingID
			if bindingID != "" {
				if _, err := env.DiskBindings.Lookup(bindingID); err != nil {
					return p, nil, batchdispatch.NewStartError(batchdispatch.CodeNoBinding, err)
				}
			}
		}
		execKey := diskOwnerKey{kind: diskOwnerExecution, instance: uuid.NewString(), bindingID: bindingID, registryID: p.RegistryID}
		if err := env.DiskBindings.acquire(execKey); err != nil {
			return p, nil, batchdispatch.NewStartError(batchdispatch.CodeNoBinding, err)
		}
		var once sync.Once
		execRelease = func() { once.Do(func() { env.DiskBindings.release(execKey) }) }
		if bindingID != "" {
			registry, err := env.Leaves(p.RegistryID)
			if err != nil || registry == nil || !registry.HasToolset(p.Policy.RestrictedToolset) {
				if err == nil {
					err = ErrDiskProviderUnavailable
				}
				execRelease()
				return p, nil, batchdispatch.NewStartError(batchdispatch.CodeNoBinding, err)
			}
			leaf, err := registry.Resolve(p.Tool)
			if err != nil {
				execRelease()
				return p, nil, batchdispatch.NewStartError(batchdispatch.CodeNoBinding, err)
			}
			_ = leaf
			provider, err := env.DiskBindings.Lookup(bindingID)
			if err != nil {
				execRelease()
				return p, nil, batchdispatch.NewStartError(batchdispatch.CodeNoBinding, err)
			}
			p.diskProvider = provider
		}
	}
	return p, execRelease, nil
}

// Leaf resolution identity comes from batchdispatch.LeafLookupID: the
// invocation's pinned registry identity when present, else the legacy
// session-keyed lookup.
func (env batchLeafEnv) resolveLeaf(p batchLeafPayload) (fantasy.AgentTool, error) {
	if env.Leaves == nil {
		return nil, batchdispatch.NewStartError(batchdispatch.CodeNoLeaf, errors.New("no toolset provider"))
	}
	registry, err := env.Leaves(batchdispatch.LeafLookupID(p.RegistryID, p.SessionID))
	if err != nil {
		return nil, batchdispatch.NewStartError(batchdispatch.CodeNoLeaf, err)
	}
	tool, err := registry.Resolve(p.Tool)
	if err != nil {
		return nil, batchdispatch.NewStartError(batchdispatch.CodeNoLeaf, err)
	}
	return tool, nil
}

// batchArgvFamily is inert until explicitly registered with a batchExecutor.
// It uses the pinned policy leaf, never a raw run_command factory.
type batchArgvFamily struct{}

var _ batchLeafFamily = batchArgvFamily{}

func (batchArgvFamily) start(ctx context.Context, env batchLeafEnv, l jobbatch.Launch, p batchLeafPayload, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.Tool != tools.RunCommandToolName {
		return nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, errors.New("argv requires run_command"))
	}
	if p.Policy != nil && p.Policy.RoleContract != nil && !roleContractAllowsTool(p.Policy.RoleContract, p.Tool) {
		return nil, batchdispatch.NewStartError(batchdispatch.CodeNoLeaf, fmt.Errorf("role tool denied: %s", p.Tool))
	}
	leaf, err := env.resolveLeaf(p)
	if err != nil {
		return nil, err
	}
	if leaf == nil || leaf.Info().Name != tools.RunCommandToolName {
		return nil, batchdispatch.NewStartError(batchdispatch.CodeNoLeaf, errors.New("pinned argv leaf is not run_command"))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Acceptance is the boundary: subsequent caller cancellation cannot reject
	// setup and abandon a process. Values survive, lifetime belongs to Stop.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// R11F round-2 (P1-5c): the PINNED policy wins — rebuilt exclusively
	// from the payload's immutable policy transport.
	runCtx = batchLeafRunContext(runCtx, p)
	if p.Policy != nil && p.Policy.RoleContract != nil {
		runCtx = toolwrap.WithRoleToolContract(runCtx, p.Policy.RoleContract)
	}
	if p.SelectedRoleContract != nil {
		runCtx = WithSelectedRoleContract(runCtx, p.SelectedRoleContract)
	}
	e := newRetainedArgv(cancel, env.OutputCache, l.ID, batchmeta.OutputRoot(l.ID))
	runCtx = e.WithArgvHandle(runCtx)
	go e.Run(runCtx, leaf, fantasy.ToolCall{ID: p.ToolCallID, Name: p.Tool, Input: string(p.Input)}, report)
	return e, nil
}

func newRetainedArgv(cancel context.CancelFunc, cache *batchOutputCache, id, root jobbatch.ID) *batchexec.ArgvExecution {
	output := batchExecutionOutput(cache, id, root, batchmeta.OutputCursorArgvRestart)
	var capture func(batchexec.ArgvHandle)
	if cache != nil {
		capture = func(p batchexec.ArgvHandle) {
			_ = batchOutputCapture(cache, id, root, batchArgvOutputReader{p}, batchmeta.OutputCursorArgvRestart)
		}
	}
	return batchexec.NewArgv(cancel, output, capture)
}

type batchShellFamily struct{}

var _ batchLeafFamily = batchShellFamily{}

func (batchShellFamily) start(ctx context.Context, env batchLeafEnv, l jobbatch.Launch, p batchLeafPayload, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.Tool != tools.BashToolName {
		return nil, batchdispatch.NewStartError(batchdispatch.CodeBadPayload, errors.New("shell kind requires bash"))
	}
	if p.Policy != nil && p.Policy.RoleContract != nil && !roleContractAllowsTool(p.Policy.RoleContract, p.Tool) {
		return nil, batchdispatch.NewStartError(batchdispatch.CodeNoLeaf, fmt.Errorf("role tool denied: %s", p.Tool))
	}
	leaf, err := env.resolveLeaf(p)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// R11F round-2 (P1-5c): the PINNED policy wins — rebuilt exclusively
	// from the payload's immutable policy transport.
	runCtx = batchLeafRunContext(runCtx, p)
	if p.Policy != nil && p.Policy.RoleContract != nil {
		runCtx = toolwrap.WithRoleToolContract(runCtx, p.Policy.RoleContract)
	}
	if p.SelectedRoleContract != nil {
		runCtx = WithSelectedRoleContract(runCtx, p.SelectedRoleContract)
	}
	e := newRetainedShell(cancel, env.OutputCache, l.ID, batchmeta.OutputRoot(l.ID))
	runCtx = e.WithShellHandle(runCtx)
	go e.Run(runCtx, leaf, fantasy.ToolCall{ID: p.ToolCallID, Name: p.Tool, Input: string(p.Input)}, report)
	return e, nil
}

func newRetainedShell(cancel context.CancelFunc, cache *batchOutputCache, id, root jobbatch.ID) *batchexec.ShellExecution {
	output := batchExecutionOutput(cache, id, root, batchmeta.OutputCursorClamp)
	var capture func(jobbatch.OutputReader)
	if cache != nil {
		capture = func(reader jobbatch.OutputReader) {
			_ = batchOutputCapture(cache, id, root, reader, batchmeta.OutputCursorClamp)
		}
	}
	return batchexec.NewShell(cancel, output, capture)
}
