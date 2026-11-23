package agent

import (
	"context"
	"errors"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore/modelcontrol"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	toolwrap "github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
)

type batchModelDelivery interface {
	DeliverBatch(context.Context, jobbatch.ID, BatchInvocation) (fantasy.ToolResponse, error)
}

type installedBatchModelControl struct {
	outputCache        *batchOutputCache
	runner             batchModelRunner
	delivery           batchModelDelivery
	store              *session.BatchStore
	live               *session.BatchLiveReader
	sessions           coresession.Service
	host, runnerID     string
	profile            *BatchProfile
	resolver           batchinst.MCPCapabilityResolver
	rootLeases         *batchRootLeaseStore
	runMu              *sync.Mutex
	maxDepth, maxNodes int
}

var _ BatchModelControl = (*installedBatchModelControl)(nil)

func NewInstalledBatchModelControl(runner batchModelRunner, delivery batchModelDelivery, store *session.BatchStore, live *session.BatchLiveReader, sessions coresession.Service, host, runnerID string, profile *BatchProfile, resolver batchinst.MCPCapabilityResolver, leases ...*batchRootLeaseStore) BatchModelControl {
	control := &installedBatchModelControl{runner: runner, delivery: delivery, store: store, live: live, sessions: sessions, host: host, runnerID: runnerID, profile: profile, resolver: resolver, runMu: &sync.Mutex{}, maxDepth: batchcore.DefaultMaxDepth, maxNodes: batchcore.DefaultMaxNodes}
	if adapter, ok := runner.(interface{ OutputCache() *batchOutputCache }); ok {
		control.outputCache = adapter.OutputCache()
	}
	if len(leases) > 0 {
		control.rootLeases = leases[0]
	}
	return control
}

func (c *installedBatchModelControl) InstallBatchModelControls(reg *toolwrap.PolicyLeafRegistry, inv BatchInvocation) ([]fantasy.AgentTool, error) {
	if reg == nil {
		return nil, errors.New("batch controls require calling pinned registry")
	}
	return newBatchModelTools(c, inv, reg), nil
}

func (c *installedBatchModelControl) Run(ctx context.Context, inv BatchInvocation, reg *toolwrap.PolicyLeafRegistry, raw modelcontrol.Spec) (jobbatch.ID, error) {
	return c.modelMutations().Run(ctx, inv, reg, raw)
}

func (c *installedBatchModelControl) Add(ctx context.Context, caller, root, parent string, inv BatchInvocation, reg *toolwrap.PolicyLeafRegistry, items []modelcontrol.Item) ([]jobbatch.ID, error) {
	return c.modelMutations().Add(ctx, caller, root, parent, inv, reg, items)
}

func (c *installedBatchModelControl) modelMutations() modelcontrol.Mutations[BatchInvocation] {
	deps := modelcontrol.Mutations[BatchInvocation]{
		Access: c.modelAccess(), Runner: c.runner, RunMu: c.runMu,
		Invocation: func(inv BatchInvocation) modelcontrol.MutationInvocation {
			return modelcontrol.MutationInvocation{
				SessionID: inv.SessionID, CallID: inv.CallID, RegistryID: inv.RegistryID,
				PolicyRegistryID: inv.Policy.RegistryID, RestrictedToolset: inv.Policy.RestrictedToolset, DiskBindingID: inv.Policy.DiskBindingID,
				AgentDepth: inv.AgentDepth, AgentDepthSet: inv.AgentDepthSet, WaiterRank: inv.WaiterRank,
				RoleConfig: inv.roleConfig, RoleContract: inv.Policy.RoleContract,
			}
		},
		Configure: func(inv *BatchInvocation, depth int, cfg *config.Config) {
			inv.AgentDepth, inv.AgentDepthSet, inv.roleConfig = depth, true, cfg
		},
		Validate: (*BatchInvocation).Validate, ParentRoleConfig: parentRoleConfigFrom, DefaultDepth: DefaultAgentDepth,
		Build: func(raw modelcontrol.Spec, inv BatchInvocation, reg *toolwrap.PolicyLeafRegistry, depth int) (jobbatch.Spec, error) {
			inv.AgentDepth = depth
			return modelBatchSpecForAdmission(raw, inv, reg, c.resolver)
		},
		AdmitRoles: admitBatchRoleContracts, RewriteLeaf: rewriteModelBatchLeaf, Measure: measureModelBatchSpecs,
		ValidateSubmit: func(rank int, spec jobbatch.Spec) error {
			return ValidateSubmit(c.profile, batchinst.RankContext{Rank: rank}, nil, spec, c.maxDepth, c.maxNodes, c.resolver)
		},
		ValidateAdd: func(rank int, root jobbatch.ID, spec jobbatch.Spec, depth, used int) error {
			return ValidateAdd(c.profile, batchinst.RankContext{Rank: rank, Root: root}, nil, spec, depth, used, c.maxDepth, c.maxNodes, c.resolver)
		},
		StampCredentials: stampBatchCallCredentials, Hash: batchInvocationHash,
		RoleUnconfigured: ErrAgentRoleUnconfigured, DiskUnavailable: ErrDiskProviderUnavailable, MissingCallID: errBatchMissingCallID,
	}
	if c.store != nil {
		deps.Probe = c.store.BatchProbeLive
	}
	if c.delivery != nil {
		deps.Deliver = c.delivery.DeliverBatch
	}
	if l := c.rootLeases; l != nil {
		deps.Leases = modelcontrol.RootLeases{Matches: l.Matches, Acquire: l.acquire, Release: l.Release, Adopt: l.Adopt}
	}
	return deps
}

// modelAccess adapts the installed root dependencies without storing a second
// copy of their state. In particular, typed nil readers must remain absent.
func (c *installedBatchModelControl) modelAccess() modelcontrol.Access {
	var store modelcontrol.Store
	if c.store != nil {
		store = c.store
	}
	var live modelcontrol.LiveReader
	if c.live != nil {
		live = c.live
	}
	return modelcontrol.NewAccess(store, live, c.sessions, c.runner, c.outputCache.readWithMetadata, c.host, c.runnerID)
}

func (c *installedBatchModelControl) StatusNode(ctx context.Context, caller, root, node string) (any, error) {
	access := c.modelAccess()
	return access.StatusNode(ctx, caller, root, node)
}

func (c *installedBatchModelControl) Output(ctx context.Context, caller, root, node string, cursor uint64) (any, error) {
	access := c.modelAccess()
	return access.Output(ctx, caller, root, node, cursor)
}

func (c *installedBatchModelControl) Stop(ctx context.Context, caller, root, node string) (any, error) {
	access := c.modelAccess()
	return access.Stop(ctx, caller, root, node)
}
