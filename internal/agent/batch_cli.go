package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/google/uuid"
)

// BatchCLITask is the untrusted external task form. Payloads, registry IDs,
// sessions and tool-call identities are deliberately not accepted from JSON.
type BatchCLITask struct {
	Tool                 string                `json:"tool"`
	Input                json.RawMessage       `json:"input"`
	AgentDepth           *int                  `json:"agent_depth,omitempty"`
	SelectedRoleContract *session.RoleContract `json:"selected_role_contract,omitempty"`
}

type batchCLIBinding struct {
	registry     *toolwrap.PolicyLeafRegistry
	id           string
	session      string
	message      string
	images       bool
	model        string
	allow        *permission.RunAllowlistSpec
	scope        *buildResourceScope
	folder       *permission.FolderScopeSpec
	roleContract *session.RoleContract
	config       *config.Config
}

// BatchCLIBuilder binds trusted constructors to a concrete coordinator.
// QuestionRoute is the intake's trusted answer-route decision, made once per
// builder: true stamps batch leaves with a held-question answer route, false
// leaves them unattended.
type BatchCLIBuilder struct {
	coord         *coordinator
	ingress       BatchWorkIngress
	questionRoute bool
}

// NewBatchCLIBuilder requires the installed ingress and the concrete Rush
// coordinator, ensuring trees execute through its pinned policy tools.
// Its callers are the served intake and parent-owned flows whose surfaces
// carry an answer route; only NewBatchCLIBuilderWithQuestionRoute may stamp
// the unattended (no-route) decision.
func NewBatchCLIBuilder(c Coordinator, in BatchWorkIngress) (*BatchCLIBuilder, error) {
	return NewBatchCLIBuilderWithQuestionRoute(c, in, true)
}

func (b *BatchCLIBuilder) bind(ctx context.Context, sessionID, messageID string) (batchCLIBinding, error) {
	if b == nil || b.coord == nil || b.ingress == nil || sessionID == "" || messageID == "" {
		return batchCLIBinding{}, errors.New("batch CLI: missing installed runtime or owner identity")
	}
	cfg, _ := b.coord.cfg.Snapshot()
	coder, ok := cfg.Agents[config.AgentCoder]
	if !ok {
		return batchCLIBinding{}, errors.New("batch CLI: coder tool configuration unavailable")
	}
	// Give this deferred CLI construction its own release scope while
	// preserving any call policy already carried by the caller context.
	ctx = WithParentRoleConfigSnapshot(ctx, cfg)
	owned := CallOptions{}
	if existing := callOptionsFrom(ctx); existing != nil {
		if existing.RoleContract != nil {
			owned.RoleContract = existing.RoleContract.Clone()
		}
		if tools.IsCustomDiskProvider(existing.DiskProvider) || existing.DiskBindingID != "" || existing.DiskBinding != nil {
			return batchCLIBinding{}, errors.New("batch CLI does not support caller-supplied disk providers")
		}
		owned = *existing
		if existing.RoleContract != nil {
			owned.RoleContract = existing.RoleContract.Clone()
		}
	}
	if !owned.AgentDepthSet {
		owned.MaxAgentDepth = DefaultAgentDepth
		owned.AgentDepthSet = true
	}
	scope := newBuildResourceScope()
	owned.BuildScope = scope
	ctx = WithCallOptions(ctx, &owned)
	if scopeSpec := folderScopeSpecFrom(ctx); scopeSpec != nil {
		ctx = WithFolderScopeSpec(ctx, scopeSpec)
	}
	// The toolset embeds a sub-agent whose prompt and tool builds run
	// asynchronously. Build them on a gate of their own and drain it while ctx
	// (the submitting request) is still live: on the coordinator's shared gate
	// a build outliving the request fails with context.Canceled, and that
	// sticky error would then fail every later run that waits on the gate.
	buildGate := &readyGate{}
	ctx = withReadyGate(ctx, buildGate)
	built, err := b.coord.buildPolicyTools(ctx, cfg, coder, false)
	if waitErr := buildGate.Wait(); err == nil {
		err = waitErr
	}
	if err != nil {
		scope.Close()
		return batchCLIBinding{}, err
	}
	id := built.RegistryID
	if id == "" {
		scope.Close()
		return batchCLIBinding{}, errors.New("batch CLI: policy tool construction did not acquire an owner")
	}
	var allow *permission.RunAllowlistSpec
	if v := runAllowlistSpecFrom(ctx); v != nil {
		copy := *v
		allow = &copy
	}
	var folder *permission.FolderScopeSpec
	if v := folderScopeSpecFrom(ctx); v != nil {
		copy := *v
		folder = &copy
	}
	return batchCLIBinding{
		registry: built.Leaves, id: id, session: sessionID, message: messageID,
		allow: allow, scope: scope, folder: folder, roleContract: cloneRoleContractForCall(parentRoleContractFrom(ctx)), config: cfg,
	}, nil
}

func (b *BatchCLIBuilder) translate(ctx context.Context, bind batchCLIBinding, spec jobbatch.Spec) (jobbatch.Spec, error) {
	if spec.Task != nil {
		var external struct {
			Tool                 string                `json:"tool"`
			Input                json.RawMessage       `json:"input"`
			AgentDepth           *int                  `json:"agent_depth,omitempty"`
			SelectedRoleContract *session.RoleContract `json:"selected_role_contract,omitempty"`
		}
		if err := json.Unmarshal(spec.Task.Payload, &external); err != nil {
			return jobbatch.Spec{}, fmt.Errorf("batch CLI task payload must be {tool,input}: %w", err)
		}
		task := BatchCLITask{Tool: external.Tool, Input: external.Input, AgentDepth: external.AgentDepth, SelectedRoleContract: external.SelectedRoleContract}
		if task.Tool == "" || len(task.Input) == 0 || !json.Valid(task.Input) {
			return jobbatch.Spec{}, errors.New("batch CLI task requires tool and valid input")
		}
		if batchControlTarget(task.Tool) || !batchWorkTarget(task.Tool) {
			return jobbatch.Spec{}, fmt.Errorf("batch CLI: tool %q is unavailable or control-plane; use a supported work tool", task.Tool)
		}
		var selectedRoleContract *session.RoleContract
		if task.Tool == modelkit.AgentToolName {
			if err := validateAgentRoleInput(task.Input); err != nil {
				return jobbatch.Spec{}, err
			}
			var requested struct {
				Role RequestedModelRole `json:"role"`
			}
			if err := json.Unmarshal(task.Input, &requested); err != nil {
				return jobbatch.Spec{}, err
			}
			selectedRoleContract = cloneRoleContractForCall(task.SelectedRoleContract)
			if requested.Role != "" && !requested.Role.Builtin() {
				if err := batchinst.ValidateSelectedRole(selectedRoleContract, string(requested.Role), bind.roleContract); err != nil {
					return jobbatch.Spec{}, err
				}
			} else if selectedRoleContract != nil {
				return jobbatch.Spec{}, fmt.Errorf("%w: custom snapshot does not match built-in or omitted role", ErrAgentRoleMismatch)
			}
		} else if task.SelectedRoleContract != nil {
			return jobbatch.Spec{}, fmt.Errorf("%w: selected role snapshot on non-agent leaf", ErrAgentRoleMismatch)
		}
		leaf, err := bind.registry.Resolve(task.Tool)
		if err != nil {
			return jobbatch.Spec{}, fmt.Errorf("batch CLI: tool %q is not in configured pinned policy toolset: %w", task.Tool, err)
		}
		// Resolve the pinned decorated tool solely to ensure it exists. The
		// executor runs its policy-preserving leaf with the same run context.
		_ = leaf
		depth := DefaultAgentDepth
		if opts := callOptionsFrom(ctx); opts != nil && opts.AgentDepthSet {
			depth = opts.MaxAgentDepth
		}
		if task.AgentDepth != nil {
			if *task.AgentDepth < 0 || *task.AgentDepth > depth {
				return jobbatch.Spec{}, errors.New("batch max_agent_depth exceeds trusted caller cap")
			}
			depth = *task.AgentDepth
		}
		payload, err := json.Marshal(batchLeafPayload{
			Tool: task.Tool, ToolCallID: uuid.NewString(), SessionID: bind.session, Input: task.Input, RegistryID: bind.id,
			Policy:               &batchLeafPolicyContext{RegistryID: bind.id, MessageID: bind.message, SupportsImages: bind.images, ModelName: bind.model, FolderScope: bind.folder, RunAllowlist: bind.allow, RestrictedToolset: bind.id, RoleContract: cloneRoleContractForCall(bind.roleContract)},
			SelectedRoleContract: selectedRoleContract,
			QuestionRoute:        b.questionRoute,
			AgentDepth:           &depth,
		})
		if err != nil {
			return jobbatch.Spec{}, err
		}
		group, _, err := batchinst.ClassifyLeaf(batchLeafKind(task.Tool), task.Tool, batchinst.MCPCapability{}, batchAdmissionPolicy())
		if err != nil {
			return jobbatch.Spec{}, err
		}
		spec.Task.Kind = batchLeafKind(task.Tool)
		spec.Task.Group = group
		spec.Task.Payload = payload
		return spec, nil
	}
	if spec.Batch == nil {
		return jobbatch.Spec{}, errors.New("batch CLI: every item must be exactly a task or batch")
	}
	for i := range spec.Batch.Items {
		item, err := b.translate(ctx, bind, spec.Batch.Items[i])
		if err != nil {
			return jobbatch.Spec{}, err
		}
		spec.Batch.Items[i] = item
	}
	return spec, nil
}

// TranslateSpec turns an external structural tree into the engine tree with
// trusted session/message and pinned policy identity supplied by Rush.
func (b *BatchCLIBuilder) TranslateSpec(ctx context.Context, sessionID, messageID string, spec jobbatch.Spec) (jobbatch.Spec, func(), error) {
	var err error
	spec, err = b.admitRoles(ctx, spec)
	if err != nil {
		return jobbatch.Spec{}, nil, err
	}
	ctx = WithAdmittedBatchRoleContracts(ctx)
	bind, err := b.bind(ctx, sessionID, messageID)
	if err != nil {
		return jobbatch.Spec{}, nil, err
	}
	if spec.Batch == nil || spec.Task != nil {
		bind.scope.Close()
		return jobbatch.Spec{}, nil, errors.New("batch CLI run requires batch root")
	}
	translated, err := b.translate(ctx, bind, spec)
	if err != nil {
		bind.scope.Close()
		return jobbatch.Spec{}, nil, err
	}
	return translated, bind.scope.Close, nil
}

func (b *BatchCLIBuilder) admitRoles(ctx context.Context, spec jobbatch.Spec) (jobbatch.Spec, error) {
	if b == nil || b.coord == nil {
		return jobbatch.Spec{}, errors.New("batch CLI: installed coordinator is required")
	}
	if batchinst.RoleContractsWereAdmitted(ctx) {
		if err := validateBatchRolePayloads(spec); err != nil {
			return jobbatch.Spec{}, err
		}
		return spec, nil
	}
	cfg, _ := b.coord.cfg.Snapshot()
	return admitBatchRoleContracts(cfg, parentRoleContractFrom(ctx), spec)
}

// TranslateItems applies an already-pinned identity to Add children.
func (b *BatchCLIBuilder) TranslateItems(ctx context.Context, sessionID, messageID string, items []jobbatch.Spec) ([]jobbatch.Spec, func(), error) {
	var err error
	for i := range items {
		items[i], err = b.admitRoles(ctx, items[i])
		if err != nil {
			return nil, nil, err
		}
	}
	ctx = WithAdmittedBatchRoleContracts(ctx)
	bind, err := b.bind(ctx, sessionID, messageID)
	if err != nil {
		return nil, nil, err
	}
	out := make([]jobbatch.Spec, len(items))
	for i := range items {
		out[i], err = b.translate(ctx, bind, items[i])
		if err != nil {
			bind.scope.Close()
			return nil, nil, err
		}
	}
	return out, bind.scope.Close, nil
}

type BatchExternalSpec = batchinst.BatchExternalSpec

// Keep the CLI and delegation defaults equal until callers share one owner.
var (
	_ [DefaultAgentDepth - batchinst.DefaultAgentDepth]struct{}
	_ [batchinst.DefaultAgentDepth - DefaultAgentDepth]struct{}
)

func ParseBatchExternalSpecJSON(raw []byte) (jobbatch.Spec, error) {
	return batchinst.ParseBatchExternalSpecJSON(raw)
}

func ParseBatchExternalSpecJSONWithAgentDepth(raw []byte, trusted int) (jobbatch.Spec, error) {
	return batchinst.ParseBatchExternalSpecJSONWithAgentDepth(raw, trusted)
}

// Invoke is the ONE ingress API (decomposition row C): validate the
// invocation fail-closed, refuse D1 control tools as generic targets, admit
// the one-item spec through the R11A validator, attach to a live root for a
// matching retry (exactly-once execution), otherwise Submit, then hand the
// started root to the R11D delivery seam. Hooks, restrictions and permissions
// run once, inside the pinned policy leaf the executor resolves; the old
// wrapper path is never taken.
//
// A nil delivery seam or runner handle is a typed not-wired error returned
// BEFORE any store probe or engine call: the dormant runtime never falls back
// to the legacy async path.
func (r *batchRuntime) Invoke(ctx context.Context, inv BatchInvocation, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	spec, resp, err := batchcore.PrepareDirectCall(ctx, leaf, call, batchcore.DirectCallPreparation{
		Validate: inv.Validate, ControlTarget: batchControlTarget,
		CanonicalInput: batchCanonicalInput, StampCredentials: stampBatchCallCredentials,
		AgentDepth: &inv.AgentDepth, AgentDepthSet: &inv.AgentDepthSet,
		DefaultDepth: DefaultAgentDepth, MaxDepth: MaxAgentDepthForResume,
		MissingCallID: errBatchMissingCallID, NilLeaf: errBatchNilLeaf, ControlPlaneLeaf: batchinst.ErrBatchControlPlaneLeaf,
		BuildSpec: func(input json.RawMessage) (jobbatch.Spec, error) {
			return batchOneItemSpec(inv, call, input)
		},
	})
	if spec.Task == nil {
		return resp, err
	}
	hash := batchInvocationHash(spec)

	r.mu.Lock()
	delivery, runner := r.cfg.Delivery, r.cfg.Runner
	r.mu.Unlock()
	if delivery == nil {
		return fantasy.ToolResponse{}, fmt.Errorf("%w", errBatchDeliveryNotWired)
	}
	if runner == nil {
		return fantasy.ToolResponse{}, fmt.Errorf("%w", errBatchRunnerNotWired)
	}
	probe := batchcore.AttachProbe{
		Owner: inv.SessionID, Host: r.cfg.Host, RunnerID: r.cfg.RunnerID,
		ToolCallID: call.ID, InputHash: hash,
		SubmittedMessageID: inv.MessageID, SubmittedRegistryID: inv.RegistryID,
	}
	fresh := batchcore.FreshCall{
		Attach: r.cfg.Attach, LeaseUnavailable: ErrDiskProviderUnavailable,
		ReleaseHandoff: func(ctx context.Context) {
			if handoff := toolwrap.HandoffFrom(ctx); handoff != nil {
				handoff.Release()
			}
		},
		Submit: func(ctx context.Context) (jobbatch.ID, error) { return r.submit(ctx, runner, inv, spec, call.ID) },
		Deliver: func(ctx context.Context, root jobbatch.ID) (fantasy.ToolResponse, error) {
			return delivery.DeliverBatch(ctx, root, inv)
		},
	}
	if inv.Policy.DiskBindingID != "" {
		fresh.MatchLease = func(root jobbatch.ID) bool {
			return r.cfg.RootLeases != nil && r.cfg.RootLeases.Matches(root, inv.Policy.DiskBindingID, inv.RegistryID)
		}
	}
	return r.core.Invoke(ctx, probe, fresh, errBatchInvocationMismatch)
}

// submit adapts the root invocation and runtime wiring to batchcore.
func (r *batchRuntime) submit(ctx context.Context, runner batchcore.RunnerHandle, inv BatchInvocation, spec jobbatch.Spec, callID string) (jobbatch.ID, error) {
	params := batchcore.SubmitParams{
		Owner: inv.SessionID, RegistryID: inv.RegistryID, DiskBindingID: inv.Policy.DiskBindingID,
		CallID: callID, Spec: spec, Runner: runner,
		Admit: func(root jobbatch.ID, spec jobbatch.Spec) error {
			waiter := batchinst.RankContext{Rank: inv.WaiterRank, Root: root}
			return ValidateSubmit(r.cfg.Profile, waiter, nil, spec, r.cfg.MaxDepth, r.cfg.MaxNodes, r.cfg.Resolver)
		},
		InvalidAdmission: errBatchSubmitBeforeInvalid,
		LeaseUnavailable: ErrDiskProviderUnavailable,
		HasDiskBindings:  r.cfg.DiskBindings != nil,
		HandoffFrom:      toolwrap.HandoffFrom,
	}
	if leases := r.cfg.RootLeases; leases != nil {
		params.AcquireRoot = leases.acquire
		params.ReleaseRoot = leases.Release
		params.AdoptRoot = leases.Adopt
	}
	return batchcore.Submit(ctx, params)
}

func batchOneItemSpec(inv BatchInvocation, call fantasy.ToolCall, input json.RawMessage) (jobbatch.Spec, error) {
	return batchcore.OneItemSpec(call, input, inv.AgentDepth, func(role string) (*session.RoleContract, error) {
		cfg, err := configForBatchInvocation(inv)
		if err != nil {
			return nil, err
		}
		return ResolveRoleContract(cfg, role, inv.Policy.RoleContract)
	}, func(fields batchcore.DirectLeafFields) (json.RawMessage, error) {
		payload := batchLeafPayload{
			Tool: call.Name, ToolCallID: call.ID, SessionID: inv.SessionID,
			Input: input, RegistryID: inv.RegistryID, Policy: &inv.Policy,
			QuestionRoute: inv.QuestionRoute, SelectedRoleContract: fields.SelectedRoleContract,
			AgentDepth: fields.AgentDepth,
		}
		if fields.WakeOnlySeconds != nil {
			payload.Timeout = &batchLeafTimeoutSpec{Kind: batchinst.TimeoutWakeOnly, Seconds: *fields.WakeOnlySeconds}
		}
		return json.Marshal(payload)
	}, lookupBatchLeafKindOverride)
}

func batchLeafKind(tool string) string {
	return batchcore.LeafKind(tool, lookupBatchLeafKindOverride)
}

func lookupBatchLeafKindOverride(tool string) (string, bool) {
	if override := batchLeafKindOverride.Load(); override != nil {
		return (*override)(tool)
	}
	return "", false
}

// batchLeafKindOverride is set only by the flag-on coordinator binding. It is
// atomic: every App bind stores it while invocations of other Apps (and
// parallel tests) read it.
var batchLeafKindOverride atomic.Pointer[func(tool string) (kind string, ok bool)]

// setBatchLeafKindOverride installs fn (nil clears the override).
func setBatchLeafKindOverride(fn func(tool string) (kind string, ok bool)) {
	if fn == nil {
		batchLeafKindOverride.Store(nil)
		return
	}
	batchLeafKindOverride.Store(&fn)
}

// Delivery modes of one invocation. R11D owns the elections; the ingress only
// validates the closed set and carries the choice.
const (
	batchDeliverySync     = "sync"     // The caller waits for the real completion.
	batchDeliveryInline   = "inline"   // Durable completion or started history.
	batchDeliveryDetached = "detached" // Started now; notices selected later.
)

// Background-escape policies. Escape switches delivery of the existing root,
// never its execution (R11D).
const (
	batchEscapeNever = "never" // The leaf waits for its real Done.
	batchEscapeAuto  = "auto"  // The host may switch delivery to detached.
)

// Typed invocation rejections. All are sentinels distinguishable via errors.Is.
var (
	errBatchMissingSessionID    = errors.New("batch invocation: missing session id")
	errBatchMissingMessageID    = errors.New("batch invocation: missing message id")
	errBatchMissingCallID       = errors.New("batch invocation: missing tool call id")
	errBatchMissingRegistryID   = errors.New("batch invocation: missing pinned registry identity")
	errBatchMissingOrigin       = errors.New("batch invocation: missing origin")
	errBatchMissingPolicy       = errors.New("batch invocation: missing trusted policy context")
	errBatchBadDeliveryMode     = errors.New("batch invocation: unknown delivery mode")
	errBatchBadEscapePolicy     = errors.New("batch invocation: unknown background-escape policy")
	errBatchBadWaiterRank       = errors.New("batch invocation: waiter rank outside the closed profile ranks")
	errBatchBadDiskBinding      = errors.New("batch invocation: malformed disk binding id")
	errBatchNilLeaf             = errors.New("batch invocation: nil leaf tool")
	errBatchNilInput            = errors.New("batch invocation: nil tool input")
	errBatchInvocationMismatch  = errors.New("batch invocation: input hash mismatch for a live tool call")
	errBatchDeliveryNotWired    = errors.New("batch delivery seam is not wired")
	errBatchRunnerNotWired      = errors.New("batch runner handle is not wired")
	errBatchAttachNotWired      = errors.New("batch attach store is not wired")
	errBatchLeavesNotWired      = errors.New("batch pinned-leaf resolver is not wired")
	errBatchProfileNotWired     = errors.New("batch runtime profile is not wired")
	errBatchSubmitBeforeInvalid = errors.New("batch runtime: admission failed before the runner was called")
)

// batchLeafPolicyContext is the immutable executor-side policy transport: the
// trusted restricted-run and execution context of ONE invocation, carried in
// the task payload so concurrent calls with different folder scopes, disks or
// restricted toolsets never collide on a session-keyed lookup. It carries
// everything batch_exec_generic.go rebuilds from the caller context today
// (message id, image support, model name) plus the values it loses (the
// pinned registry identity, the folder scope, the restricted-run allowlist
// and the custom disk identity).
type batchLeafPolicyContext struct {
	RegistryID     string                       `json:"registry_id"`
	MessageID      string                       `json:"message_id,omitempty"`
	SupportsImages bool                         `json:"supports_images,omitempty"`
	ModelName      string                       `json:"model_name,omitempty"`
	FolderScope    *permission.FolderScopeSpec  `json:"folder_scope,omitempty"`
	RunAllowlist   *permission.RunAllowlistSpec `json:"run_allowlist,omitempty"`
	CustomDisk     string                       `json:"custom_disk,omitempty"`
	DiskProvider   bool                         `json:"disk_provider,omitempty"`
	// DiskBindingID is the REQUIRED process-bound provider binding
	// (disk_provider_binding.go) when this invocation carries a custom
	// DiskProvider; empty means no custom disk. Liveness is the
	// executor's job; the ID flows into the canonical spec payload hash.
	DiskBindingID     string `json:"disk_binding_id,omitempty"`
	RestrictedToolset string `json:"restricted_toolset"`
	// RoleContract is the trusted parent's immutable tool ceiling. It is
	// carried separately from a leaf's selected role snapshot and is never
	// populated from public request fields.
	RoleContract *session.RoleContract `json:"role_contract,omitempty"`
}

// batchDeliveryChoice is the mode/window pair R11D elects on.
type batchDeliveryChoice struct {
	Mode   string `json:"mode"`
	Window string `json:"window,omitempty"` // R11D: deadline or notice window.
}

// BatchInvocation is the immutable ingress metadata of ONE tool invocation.
// It is validated fail-closed before any engine or store call and never
// derived from session-keyed lookups: the pinned registry identity travels
// with the invocation.
type BatchInvocation struct {
	// SessionID is the owner session (the live-attach owner fence).
	SessionID string `json:"session_id"`
	// MessageID is the enclosing model message.
	MessageID string `json:"message_id"`
	// RegistryID is the pinned policy-leaf registry identity (R1), NOT a
	// session key: two calls of one session with different restricted
	// toolsets carry different registries.
	RegistryID string `json:"registry_id"`
	// Origin names the ingress caller family (cli_bridge, agentic_fetch,
	// coordinator) for audit.
	Origin        string `json:"origin"`
	CallID        string `json:"call_id,omitempty"`
	AgentDepth    int    `json:"agent_depth,omitempty"`
	AgentDepthSet bool   `json:"agent_depth_set,omitempty"`

	Delivery         batchDeliveryChoice `json:"delivery"`
	BackgroundEscape string              `json:"background_escape,omitempty"`

	// WaiterNode is the awaiting node of an explicit await, empty for a
	// direct call; WaiterRank is the caller's closed-profile rank.
	WaiterNode jobbatch.ID `json:"waiter_node,omitempty"`
	WaiterRank int         `json:"waiter_rank"`

	// Policy is the trusted restricted-run/policy context the executor
	// needs to rebuild its run context. Never read from session state.
	Policy batchLeafPolicyContext `json:"policy"`
	// WorkerStopScope is trusted tool-build policy; it is never model input.
	WorkerStopScope bool `json:"-"`
	// QuestionRoute is trusted tool-build policy: the calling session's own
	// held-question answer route (top-level sessions have one — their
	// operator answers; batch incarnations inherit the durable payload
	// value). It is stamped from the call context in
	// modelInvocationAtCall and flows into new agent leaf payloads, so a
	// no-route delegation can never mint a route-ful nested batch. It is
	// never model input.
	QuestionRoute bool `json:"-"`
	// roleConfig carries the admission config snapshot for model controls.
	roleConfig *config.Config
}

// batchCallIdentity returns the live-attach key of an invocation: the owner
// session and the tool call id (call.ID), exactly the pair R11B reserves.
func batchCallIdentity(inv BatchInvocation, call fantasy.ToolCall) (string, string) {
	return inv.SessionID, call.ID
}

// batchCanonicalInput compacts the model's raw tool input once. Two retries
// of one logical call carry byte-identical canonical inputs.
func batchCanonicalInput(input string) (json.RawMessage, error) {
	if strings.TrimSpace(input) == "" {
		return nil, fmt.Errorf("%w", errBatchNilInput)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(input)); err != nil {
		return nil, fmt.Errorf("batch invocation: canonicalise input: %w", err)
	}
	return json.RawMessage(compact.Bytes()), nil
}

// batchInvocationHash derives the live-attach input hash: the canonical SHA-256
// of the accepted one-item spec, mirroring the R11B commit hook's spec hash so
// a reserved root and its ingress key always agree.
func batchInvocationHash(spec jobbatch.Spec) string {
	raw, err := json.Marshal(spec)
	if err != nil {
		raw = []byte("unmarshalable")
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Validate fails closed on any missing identity (session, message or call id,
// pinned registry identity, origin, trusted policy context) or unknown
// delivery/escape choice, and canonicalises every identity value once.
func (inv *BatchInvocation) Validate() error {
	inv.SessionID = strings.TrimSpace(inv.SessionID)
	inv.MessageID = strings.TrimSpace(inv.MessageID)
	inv.RegistryID = strings.TrimSpace(inv.RegistryID)
	inv.Origin = strings.TrimSpace(inv.Origin)
	inv.Policy.RegistryID = strings.TrimSpace(inv.Policy.RegistryID)
	inv.Policy.RestrictedToolset = strings.TrimSpace(inv.Policy.RestrictedToolset)
	inv.Policy.ModelName = strings.TrimSpace(inv.Policy.ModelName)
	inv.Policy.CustomDisk = strings.TrimSpace(inv.Policy.CustomDisk)
	inv.Policy.DiskBindingID = strings.TrimSpace(inv.Policy.DiskBindingID)
	// A non-empty binding ID is a required process binding: Validate checks
	// well-formedness only; liveness is resolved by the executor before any
	// leaf side effect (batch_exec.go).
	if inv.Policy.RoleContract != nil {
		if err := inv.Policy.RoleContract.Validate(); err != nil {
			return fmt.Errorf("%w: invalid trusted role contract: %v", errBatchMissingPolicy, err)
		}
	}
	if inv.Policy.DiskBindingID != "" && !batchWellFormedBindingID(inv.Policy.DiskBindingID) {
		return fmt.Errorf("%w: binding id %q", errBatchBadDiskBinding, inv.Policy.DiskBindingID)
	}
	if inv.AgentDepthSet && (inv.AgentDepth < 0 || inv.AgentDepth > 32) {
		return errors.New("batch invocation: agent depth must be between 0 and 32")
	}
	if inv.SessionID == "" {
		return fmt.Errorf("%w", errBatchMissingSessionID)
	}
	if inv.MessageID == "" {
		return fmt.Errorf("%w", errBatchMissingMessageID)
	}
	if inv.RegistryID == "" {
		return fmt.Errorf("%w", errBatchMissingRegistryID)
	}
	if inv.Origin == "" {
		return fmt.Errorf("%w", errBatchMissingOrigin)
	}
	if inv.Policy.RegistryID == "" || inv.Policy.RestrictedToolset == "" {
		return fmt.Errorf("%w", errBatchMissingPolicy)
	}
	switch inv.Delivery.Mode {
	case "":
		inv.Delivery.Mode = batchDeliverySync
	case batchDeliverySync, batchDeliveryInline, batchDeliveryDetached:
	default:
		return fmt.Errorf("%w: %q", errBatchBadDeliveryMode, inv.Delivery.Mode)
	}
	switch inv.BackgroundEscape {
	case "":
		inv.BackgroundEscape = batchEscapeNever
	case batchEscapeNever, batchEscapeAuto:
	default:
		return fmt.Errorf("%w: %q", errBatchBadEscapePolicy, inv.BackgroundEscape)
	}
	rankOK := false
	for _, rank := range batchGroupRanks {
		if inv.WaiterRank == rank {
			rankOK = true
		}
	}
	if !rankOK {
		return fmt.Errorf("%w: %d", errBatchBadWaiterRank, inv.WaiterRank)
	}
	return nil
}

// batchWellFormedBindingID checks binding-ID well-formedness only: opaque,
// bounded, whitespace-free. Liveness is the executor's job.
func batchWellFormedBindingID(id string) bool {
	if len(id) > 128 {
		return false
	}
	for _, r := range id {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == 0 {
			return false
		}
	}
	return true
}

func batchLeafRunContext(base context.Context, p batchLeafPayload) context.Context {
	var policy *batchdispatch.RunPolicy
	if q := p.Policy; q != nil {
		policy = &batchdispatch.RunPolicy{
			MessageID: q.MessageID, SupportsImages: q.SupportsImages, ModelName: q.ModelName,
			FolderScope: q.FolderScope, RunAllowlist: q.RunAllowlist, CustomDisk: q.CustomDisk,
		}
		if q.RoleContract != nil {
			policy.WithRole = func(ctx context.Context) context.Context { return toolwrap.WithRoleToolContract(ctx, q.RoleContract) }
		}
	}
	return batchdispatch.LeafRunContext(base, p.SessionID, p.AgentDepth, WithAgentDepth, policy, p.diskProvider)
}

func WithAdmittedBatchRoleContracts(ctx context.Context) context.Context {
	return batchinst.WithAdmittedBatchRoleContracts(ctx)
}

func configForBatchInvocation(inv BatchInvocation) (*config.Config, error) {
	return batchinst.ConfigForBatchInvocation(inv.roleConfig)
}

func admitBatchRoleContracts(cfg *config.Config, inherited *session.RoleContract, spec jobbatch.Spec) (jobbatch.Spec, error) {
	return batchinst.AdmitRoleContracts(cfg, inherited, spec, batchinst.RoleAdmissionPolicy{
		ValidateBuiltin: func(cfg *config.Config, role RequestedModelRole) error {
			_, _, err := requestedRoleSlot(cfg, session.Session{}, role)
			return err
		},
		ResolveCustom: ResolveRoleContract,
	})
}

func validateBatchRolePayloads(spec jobbatch.Spec) error {
	return batchinst.ValidateRolePayloads(spec, func(raw json.RawMessage) (batchinst.RolePayload, error) {
		var p batchLeafPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return batchinst.RolePayload{}, err
		}
		out := batchinst.RolePayload{Tool: p.Tool, Input: p.Input, SelectedRoleContract: p.SelectedRoleContract}
		if p.Policy != nil {
			out.Parent = p.Policy.RoleContract
		}
		return out, nil
	})
}

func BatchRoleSnapshotHash(spec jobbatch.Spec) string { return batchinst.BatchRoleSnapshotHash(spec) }
