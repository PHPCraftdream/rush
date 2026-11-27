package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore/modelcontrol"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

type (
	batchModelControl = modelcontrol.Control[BatchInvocation]
)

func modelInvocationAtCall(ctx context.Context, inv BatchInvocation, call fantasy.ToolCall) (BatchInvocation, error) {
	inv.SessionID = tools.GetSessionFromContext(ctx)
	inv.MessageID = tools.GetMessageFromContext(ctx)
	inv.CallID = call.ID
	inv.Policy.MessageID = inv.MessageID
	inv.Policy.SupportsImages = tools.GetSupportsImagesFromContext(ctx)
	inv.Policy.ModelName = tools.GetModelNameFromContext(ctx)
	inv.Policy.RoleContract = parentRoleContractFrom(ctx)
	if inv.Policy.RoleContract != nil {
		inv.Policy.RoleContract = inv.Policy.RoleContract.Clone()
	}
	if opts := callOptionsFrom(ctx); opts != nil {
		if opts.AgentDepthSet {
			if inv.AgentDepthSet && opts.MaxAgentDepth > inv.AgentDepth {
				return BatchInvocation{}, errors.New("agent depth cannot be increased")
			}
			inv.AgentDepth = opts.MaxAgentDepth
			inv.AgentDepthSet = true
		} else if !inv.AgentDepthSet {
			inv.AgentDepth = DefaultAgentDepth
			inv.AgentDepthSet = true
		}
		if inv.Policy.DiskBindingID == "" && tools.IsCustomDiskProvider(opts.DiskProvider) {
			if opts.DiskBindingID == "" {
				return BatchInvocation{}, fmt.Errorf("%w: a custom disk provider must carry its binding handle before the call options are shared", ErrDiskProviderUnavailable)
			}
			inv.Policy.DiskBindingID = opts.DiskBindingID
		}
		inv.Policy.DiskProvider = inv.Policy.DiskBindingID != ""
	}
	inv.Policy.FolderScope = folderScopeSpecFrom(ctx)
	inv.Policy.RunAllowlist = runAllowlistSpecFrom(ctx)
	// The calling session's own answer route: absent marker = a
	// top-level session whose operator/UI/orchestrator surface is the
	// established route; present marker = a batch incarnation
	// inheriting its durable payload value. A no-route caller therefore
	// mints only no-route agent leaves.
	inv.QuestionRoute = true
	if route, ok := batchmeta.BatchQuestionRouteFrom(ctx); ok {
		inv.QuestionRoute = route
	}
	// A template-pinned DiskBindingID propagates verbatim: nested batches
	// and Add inherit the binding, never re-resolving to another provider.
	return inv, nil
}

func newBatchModelTools(c batchModelControl, inv BatchInvocation, reg *toolwrap.PolicyLeafRegistry) []fantasy.AgentTool {
	return modelcontrol.NewTools(c, reg, func(ctx context.Context, call fantasy.ToolCall) (BatchInvocation, string, error) {
		bound, err := modelInvocationAtCall(ctx, inv, call)
		return bound, bound.SessionID, err
	}, inv.WorkerStopScope)
}

func modelBatchSpec(raw modelcontrol.Spec, inv BatchInvocation, reg *toolwrap.PolicyLeafRegistry) (jobbatch.Spec, error) {
	return buildModelBatchSpec(raw, inv, reg, nil, false)
}

func modelBatchSpecForAdmission(raw modelcontrol.Spec, inv BatchInvocation, reg *toolwrap.PolicyLeafRegistry, resolver batchinst.MCPCapabilityResolver) (jobbatch.Spec, error) {
	return buildModelBatchSpec(raw, inv, reg, resolver, true)
}

func buildModelBatchSpec(raw modelcontrol.Spec, inv BatchInvocation, reg *toolwrap.PolicyLeafRegistry, resolver batchinst.MCPCapabilityResolver, admission bool) (jobbatch.Spec, error) {
	depth := inv.AgentDepth
	if !inv.AgentDepthSet {
		depth = DefaultAgentDepth
	}
	return modelcontrol.BuildSpec(raw, depth, modelcontrol.SpecDeps{
		HasRegistry: reg != nil, MaxDepth: batchcore.DefaultMaxDepth, MaxNodes: batchcore.DefaultMaxNodes,
		Measure: measureModelBatchSpecs,
		BuildLeaf: func(it modelcontrol.Item, path string, leafCap int) (*jobbatch.TaskSpec, error) {
			return buildModelBatchLeaf(it, path, leafCap, inv, reg, resolver, admission)
		},
	})
}

func measureModelBatchSpecs(specs []jobbatch.Spec) (int, int, error) {
	depth, nodes, _, err := batchinst.SpecNodes(specs, batchAdmissionTool)
	return depth, nodes, err
}

func batchModelSpecNodes(items []modelcontrol.Item, inv BatchInvocation, reg *toolwrap.PolicyLeafRegistry, resolver batchinst.MCPCapabilityResolver) ([]jobbatch.Spec, error) {
	return modelcontrol.BuildAddNodes(items, inv.AgentDepth, inv.CallID, func(raw modelcontrol.Spec, depth int) (jobbatch.Spec, error) {
		wrapped := inv
		wrapped.AgentDepth = depth
		return modelBatchSpecForAdmission(raw, wrapped, reg, resolver)
	}, rewriteModelBatchLeaf)
}

// Keep the executor's full payload encoding so retry hashes stay byte-identical.
func rewriteModelBatchLeaf(task *jobbatch.TaskSpec, callID string, maxDepth *int) error {
	var payload batchLeafPayload
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		return err
	}
	if maxDepth != nil && (payload.Tool == modelkit.AgentToolName || payload.Tool == tools.AgenticFetchToolName) &&
		(payload.AgentDepth == nil || *payload.AgentDepth > *maxDepth) {
		return errors.New("batch_add agent leaf exceeds trusted depth")
	}
	payload.ToolCallID = callID
	raw, err := json.Marshal(payload)
	if err == nil {
		task.Payload = raw
	}
	return err
}

func buildModelBatchLeaf(it modelcontrol.Item, callPath string, leafCap int, inv BatchInvocation, reg *toolwrap.PolicyLeafRegistry, resolver batchinst.MCPCapabilityResolver, admission bool) (*jobbatch.TaskSpec, error) {
	if batchControlTarget(it.Tool) {
		return nil, fmt.Errorf("control-plane tool %q cannot be a batch leaf", it.Tool)
	}
	leaf, e := reg.ResolveForContext(toolwrap.WithRoleToolContract(context.Background(), inv.Policy.RoleContract), it.Tool)
	if e != nil || leaf == nil {
		return nil, fmt.Errorf("tool %q unavailable in calling pinned toolset", it.Tool)
	}
	input, e := batchCanonicalInput(string(it.Input))
	if e != nil {
		return nil, fmt.Errorf("tool %q input: %w", it.Tool, e)
	}
	var selectedRoleContract *session.RoleContract
	if it.Tool == modelkit.AgentToolName {
		if err := validateAgentRoleInput(input); err != nil {
			return nil, err
		}
		var selected struct {
			Role RequestedModelRole `json:"role"`
		}
		if err := json.Unmarshal(input, &selected); err != nil {
			return nil, err
		}
		if selected.Role != "" && !selected.Role.Builtin() {
			cfg, cfgErr := configForBatchInvocation(inv)
			if cfgErr != nil {
				return nil, cfgErr
			}
			selectedRoleContract, e = ResolveRoleContract(cfg, string(selected.Role), inv.Policy.RoleContract)
			if e != nil {
				return nil, e
			}
		}
	}
	// Normalize the leaf's own tool-input timeout intent once, at
	// ingress, exactly like the direct path: a terminate intent feeds
	// the SAME timeout variable the explicit string form fills, a
	// wake_only intent rides durably in the payload.
	intent, e := batchinst.ParseTimeoutParam(it.Tool, string(input))
	if e != nil {
		return nil, e
	}
	if it.Timeout != "" && intent != nil {
		// An item deadline and a nested tool-input timeout are two
		// unresolvable authorities on one leaf: refuse rather than
		// silently prefer one.
		return nil, errors.New("item timeout and the leaf tool input's timeout are ambiguous; provide at most one")
	}
	leafPayload := batchLeafPayload{Tool: it.Tool, ToolCallID: fmt.Sprintf("%s-item-%s", inv.CallID, callPath), SessionID: inv.SessionID, Input: input, RegistryID: inv.RegistryID, Policy: &inv.Policy, QuestionRoute: inv.QuestionRoute, SelectedRoleContract: cloneRoleContractForCall(selectedRoleContract)}
	if it.Tool == modelkit.AgentToolName || it.Tool == tools.AgenticFetchToolName {
		leafPayload.AgentDepth = &leafCap
	}
	timeout := time.Duration(0)
	if it.Timeout != "" {
		timeout, e = time.ParseDuration(it.Timeout)
		if e != nil || timeout <= 0 {
			return nil, errors.New("timeout must be a positive duration")
		}
	} else if intent != nil {
		if intent.Kind == batchinst.TimeoutTerminateAndWake {
			timeout = intent.Duration
		} else {
			leafPayload.Timeout = &batchLeafTimeoutSpec{
				Kind:    batchinst.TimeoutWakeOnly,
				Seconds: intent.Seconds,
			}
		}
	}
	payload, e := json.Marshal(leafPayload)
	if e != nil {
		return nil, e
	}
	kind := batchLeafKind(it.Tool)
	group := ""
	if kind == batchdispatch.KindMCP {
		if resolver != nil {
			mcp, ok := resolver(kind, it.Tool)
			if !ok {
				return nil, fmt.Errorf("%w: %q", batchinst.ErrBatchMCPCapabilityUnknown, it.Tool)
			}
			group, _, e = batchinst.ClassifyLeaf(kind, it.Tool, mcp, batchAdmissionPolicy())
			if e != nil {
				return nil, e
			}
		} else if admission {
			return nil, fmt.Errorf("%w: %q", batchinst.ErrBatchMCPCapabilityUnknown, it.Tool)
		}
	} else {
		group, _, e = batchinst.ClassifyLeaf(kind, it.Tool, batchinst.MCPCapability{}, batchAdmissionPolicy())
		if e != nil {
			return nil, e
		}
	}
	return &jobbatch.TaskSpec{Kind: kind, Group: group, Payload: payload, Timeout: timeout}, nil
}

// Batch group names. The set is closed; "" is the explicit default group.
const (
	batchGroupIO          = "io"
	batchGroupCLIDriver   = "cli_driver"
	batchGroupAgentDriver = "agent_driver"
	batchGroupDefault     = "" // The explicit default group.
)

// Batch group ranks. Lower ranks must never wait on equal or higher ranks.
// batchGroupDefault shares rank 0 with io: default-group work is plain
// non-delegating work, so it shares rank 0; it is still a distinct capped
// group.
var batchGroupRanks = map[string]int{
	batchGroupIO:          0,
	batchGroupCLIDriver:   1,
	batchGroupAgentDriver: 2,
	batchGroupDefault:     0,
}

// Typed constructor rejections. All are sentinel errors distinguishable via
// errors.Is.
var (
	errBatchCapInvalid       = errors.New("batch profile cap must be > 0")
	errBatchGlobalTooSmall   = errors.New("batch profile global cap below sum of group caps")
	errBatchMissingGroup     = errors.New("batch profile missing a required group cap")
	errBatchUnknownGroupName = errors.New("batch profile unknown group name")
	errBatchRankConflict     = errors.New("batch profile duplicate rank in rank table")
)

// BatchProfileCaps are the per-group and global caps of a profile. All group
// caps must be > 0 and Global must be >= their sum.
type BatchProfileCaps struct {
	IO          int
	CLIDriver   int
	AgentDriver int
	Default     int
	Global      int
}

// BatchProfile is an immutable closed capacity profile. It validates at
// construction and exposes only lookups; there are no setters and no exported
// mutable state.
type BatchProfile struct {
	caps   map[string]int
	ranks  map[string]int
	global int
}

// validateBatchRanks builds the fixed rank table and rejects duplicate ranks.
// The documented exception is the shared rank 0 of io and the default group:
// default-group work is plain non-delegating work, so it shares rank 0 with
// io while remaining a distinct capped group.
func validateBatchRanks(ranks map[string]int) error {
	sharedRank0 := map[string]bool{batchGroupIO: true, batchGroupDefault: true}
	seen := make(map[int]string, len(ranks))
	for group, rank := range ranks {
		if prev, ok := seen[rank]; ok {
			allow := rank == 0 && sharedRank0[prev] && sharedRank0[group]
			if !allow {
				return fmt.Errorf("%w: %q and %q both rank %d", errBatchRankConflict, prev, group, rank)
			}
			continue
		}
		seen[rank] = group
	}
	return nil
}

// NewBatchProfile validates caps and returns an immutable profile.
func NewBatchProfile(caps BatchProfileCaps) (*BatchProfile, error) {
	groups := map[string]int{
		batchGroupIO:          caps.IO,
		batchGroupCLIDriver:   caps.CLIDriver,
		batchGroupAgentDriver: caps.AgentDriver,
		batchGroupDefault:     caps.Default,
	}
	return newBatchProfileFromGroups(groups, caps.Global)
}

// NewBatchProfileFromMap validates a map-form profile. It rejects unknown
// group names, missing required groups (including "") and any cap <= 0.
func NewBatchProfileFromMap(groups map[string]int, global int) (*BatchProfile, error) {
	caps := make(map[string]int, len(groups))
	for name, cap := range groups {
		switch name {
		case batchGroupIO, batchGroupCLIDriver, batchGroupAgentDriver, batchGroupDefault:
			caps[name] = cap
		default:
			return nil, fmt.Errorf("%w: %q", errBatchUnknownGroupName, name)
		}
	}
	for _, name := range batchProfileGroups() {
		if _, ok := caps[name]; !ok {
			return nil, fmt.Errorf("%w: %q", errBatchMissingGroup, name)
		}
	}
	return newBatchProfileFromGroups(caps, global)
}

// newBatchProfileFromGroups applies the shared cap, sum and rank validation.
func newBatchProfileFromGroups(caps map[string]int, global int) (*BatchProfile, error) {
	if global <= 0 {
		return nil, fmt.Errorf("%w: global cap %d", errBatchCapInvalid, global)
	}
	sum := 0
	for name, cap := range caps {
		if cap <= 0 {
			return nil, fmt.Errorf("%w: group %q cap %d", errBatchCapInvalid, name, cap)
		}
		sum += cap
	}
	if global < sum {
		return nil, fmt.Errorf("%w: global %d < sum %d", errBatchGlobalTooSmall, global, sum)
	}
	ranks := make(map[string]int, len(batchGroupRanks))
	for name, rank := range batchGroupRanks {
		if _, ok := caps[name]; !ok {
			return nil, fmt.Errorf("%w: %q", errBatchMissingGroup, name)
		}
		ranks[name] = rank
	}
	if err := validateBatchRanks(ranks); err != nil {
		return nil, err
	}
	capsCopy := make(map[string]int, len(caps))
	for name, cap := range caps {
		capsCopy[name] = cap
	}
	return &BatchProfile{caps: capsCopy, ranks: ranks, global: global}, nil
}

// NewBatchProfileDefault returns the documented default profile.
//
// IO = 50 anchors the existing background-shell cap MaxBackgroundJobs = 50 in
// internal/shell/background.go. CLIDriver = 1, AgentDriver = 1 and Default = 1
// have no existing code anchor: they are chosen minimal, i.e. strictly serial
// rank>0 delegation and minimal default-group width, not derived from code.
// Global = 53 is the sum of the four caps, the minimum legal global cap.
func NewBatchProfileDefault() (*BatchProfile, error) {
	return NewBatchProfile(BatchProfileCaps{IO: 50, CLIDriver: 1, AgentDriver: 1, Default: 1, Global: 53})
}

// Limits maps the profile onto engine limits. The ByGroup map is a fresh copy;
// mutating it never affects the profile.
func (p *BatchProfile) Limits(maxDepth, maxNodes int) jobbatch.Limits {
	byGroup := make(map[string]int, len(p.caps))
	for name, cap := range p.caps {
		byGroup[name] = cap
	}
	return jobbatch.Limits{MaxConcurrent: p.global, ByGroup: byGroup, MaxDepth: maxDepth, MaxNodes: maxNodes}
}

// GroupCap returns the cap of a group and whether the group is in the profile.
func (p *BatchProfile) GroupCap(group string) (int, bool) {
	cap, ok := p.caps[group]
	return cap, ok
}

// GroupRank returns the rank of a group, including the default group "".
func (p *BatchProfile) GroupRank(group string) (int, bool) {
	rank, ok := p.ranks[group]
	return rank, ok
}

// Groups returns the closed group set in sorted order.
func (p *BatchProfile) Groups() []string {
	return batchProfileGroups()
}

// batchProfileGroups returns a fresh sorted copy of the closed group set.
func batchProfileGroups() []string {
	groups := make([]string, 0, len(batchGroupRanks))
	for name := range batchGroupRanks {
		groups = append(groups, name)
	}
	sort.Strings(groups)
	return groups
}
