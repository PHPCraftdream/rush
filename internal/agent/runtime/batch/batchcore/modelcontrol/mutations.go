package modelcontrol

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore"
	toolwrap "github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session/batch/sessionbatch"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
)

type MutationRunner interface {
	Submit(context.Context, jobbatch.SubmitRequest) (jobbatch.ID, error)
	Add(context.Context, jobbatch.AddRequest) ([]jobbatch.ID, error)
}

type boundRunner interface {
	SubmitBound(context.Context, jobbatch.SubmitRequest) (jobbatch.ID, *jobbatch.Batch, error)
}

type releaseRunner interface {
	Release(context.Context, jobbatch.ID) error
}

type RootLeases struct {
	Matches func(jobbatch.ID, string, string) bool
	Acquire func(jobbatch.ID, string, string, string) error
	Release func(jobbatch.ID, string, string)
	Adopt   func(jobbatch.ID, <-chan struct{}, string, string, func(context.Context, jobbatch.ID) error) error
}

type MutationInvocation struct {
	SessionID, CallID, RegistryID, PolicyRegistryID, RestrictedToolset, DiskBindingID string
	AgentDepth, WaiterRank                                                            int
	AgentDepthSet                                                                     bool
	RoleConfig                                                                        *config.Config
	RoleContract                                                                      *coresession.RoleContract
}

type Mutations[I any] struct {
	Access  Access
	Runner  MutationRunner
	Probe   func(context.Context, sessionbatch.BatchLiveProbe) (string, bool, error)
	Deliver func(context.Context, jobbatch.ID, I) (fantasy.ToolResponse, error)
	Leases  RootLeases
	RunMu   *sync.Mutex

	Invocation       func(I) MutationInvocation
	Configure        func(*I, int, *config.Config)
	Validate         func(*I) error
	ParentRoleConfig func(context.Context) (*config.Config, bool)
	DefaultDepth     int
	Build            func(Spec, I, *toolwrap.PolicyLeafRegistry, int) (jobbatch.Spec, error)
	AdmitRoles       func(*config.Config, *coresession.RoleContract, jobbatch.Spec) (jobbatch.Spec, error)
	RewriteLeaf      RewriteLeaf
	ValidateSubmit   func(int, jobbatch.Spec) error
	ValidateAdd      func(int, jobbatch.ID, jobbatch.Spec, int, int) error
	Measure          func([]jobbatch.Spec) (int, int, error)
	StampCredentials func(context.Context, jobbatch.Spec) (jobbatch.Spec, error)
	Hash             func(jobbatch.Spec) string

	RoleUnconfigured, DiskUnavailable, MissingCallID error
}

var batchModelRunFallbackMu sync.Mutex

func (c Mutations[I]) prepare(ctx context.Context, invocation *I, requiresRoleConfig func(MutationInvocation) bool) (MutationInvocation, error) {
	inv := c.Invocation(*invocation)
	if !inv.AgentDepthSet {
		inv.AgentDepth = c.DefaultDepth
	}
	if inv.RoleConfig == nil {
		if cfg, ok := c.ParentRoleConfig(ctx); ok {
			inv.RoleConfig = cfg
		}
	}
	if inv.RoleConfig == nil {
		if requiresRoleConfig(inv) {
			return MutationInvocation{}, c.RoleUnconfigured
		}
		inv.RoleConfig = &config.Config{}
	}
	c.Configure(invocation, inv.AgentDepth, inv.RoleConfig)
	if err := c.Validate(invocation); err != nil {
		return MutationInvocation{}, err
	}
	return c.Invocation(*invocation), nil
}

func (c Mutations[I]) Run(ctx context.Context, invocation I, reg *toolwrap.PolicyLeafRegistry, raw Spec) (jobbatch.ID, error) {
	if c.Runner == nil || c.Probe == nil || c.Deliver == nil {
		return "", errors.New("batch runtime unavailable")
	}
	inv, err := c.prepare(ctx, &invocation, func(inv MutationInvocation) bool {
		return inv.RoleContract != nil || modelSpecHasCustomRole(raw)
	})
	if err != nil {
		return "", err
	}
	if inv.CallID == "" || reg == nil || inv.RegistryID != inv.PolicyRegistryID || !reg.HasToolset(inv.RestrictedToolset) {
		return "", c.MissingCallID
	}
	spec, err := c.Build(raw, invocation, reg, inv.AgentDepth)
	if err != nil {
		return "", err
	}
	spec, err = c.AdmitRoles(inv.RoleConfig, inv.RoleContract, spec)
	if err != nil {
		return "", err
	}
	if err := assignModelBatchCallIDs(&spec, inv.CallID, "run", c.RewriteLeaf); err != nil {
		return "", err
	}
	if err := c.ValidateSubmit(inv.WaiterRank, spec); err != nil {
		return "", err
	}
	// Hash the exact spec the adapter submits. Credential stamping adds a
	// CallCredentials marker to agent payloads, and the durable reservation
	// hashes that stamped spec; probing the unstamped form misses live retries.
	spec, err = c.StampCredentials(ctx, spec)
	if err != nil {
		return "", err
	}
	inputHash := c.Hash(spec)
	var runMu *sync.Mutex
	if c.RunMu != nil {
		runMu = c.RunMu
	} else {
		runMu = &batchModelRunFallbackMu
	}
	runMu.Lock()
	locked := true
	defer func() {
		if locked {
			runMu.Unlock()
		}
	}()
	liveRoot, attached, err := c.Probe(ctx, sessionbatch.BatchLiveProbe{
		Owner: inv.SessionID, Host: c.Access.host, RunnerID: c.Access.runnerID,
		ToolCallID: inv.CallID, InputHash: inputHash,
	})
	if err != nil {
		return "", fmt.Errorf("batch attach: %w", err)
	}
	if attached {
		if c.Leases.Matches == nil || !c.Leases.Matches(jobbatch.ID(liveRoot), inv.DiskBindingID, inv.RegistryID) {
			return "", c.DiskUnavailable
		}
		locked = false
		runMu.Unlock()
		resp, err := c.Deliver(ctx, jobbatch.ID(liveRoot), invocation)
		if err != nil {
			return "", err
		}
		if resp.IsError {
			return "", fmt.Errorf("batch delivery failed: %s", resp.Content)
		}
		return jobbatch.ID(liveRoot), nil
	}
	root := batchcore.RootID()
	// Reservation identity is the caller's tool-call ID, so matching retries
	// attach to the live root before attempting another Runner submission.
	request := jobbatch.SubmitRequest{Root: root, Spec: spec, Ref: inv.CallID}
	// Root owner: acquired BEFORE Submit with the already-minted root ID;
	// it covers accepted-but-not-started children. Released on definite
	// rejection below; a successful Submit keeps it and the Done watcher
	// releases it when no further child can start.
	rootOwned := false
	if inv.RegistryID != "" {
		if c.Leases.Acquire == nil {
			return "", c.DiskUnavailable
		}
		if err := c.Leases.Acquire(root, inv.SessionID, inv.DiskBindingID, inv.RegistryID); err != nil {
			return "", err
		}
		rootOwned = true
	}
	releaseRoot := func() {
		if rootOwned {
			c.Leases.Release(root, inv.DiskBindingID, inv.RegistryID)
		}
	}
	var id jobbatch.ID
	var batch *jobbatch.Batch
	if rootOwned {
		bound, ok := c.Runner.(boundRunner)
		if !ok {
			releaseRoot()
			return "", c.DiskUnavailable
		}
		id, batch, err = bound.SubmitBound(ctx, request)
	} else {
		id, err = c.Runner.Submit(ctx, request)
	}
	if err != nil {
		releaseRoot()
		return "", err
	}
	if id != root {
		releaseRoot()
		return "", fmt.Errorf("runner root %q differs from requested %q", id, root)
	}
	if rootOwned {
		if batch == nil {
			releaseRoot()
			return "", c.DiskUnavailable
		}
		if batch.ID() != root {
			releaseRoot()
			return "", fmt.Errorf("runner root %q differs from requested %q", batch.ID(), root)
		}
		release, ok := c.Runner.(releaseRunner)
		if !ok {
			return "", c.DiskUnavailable
		}
		if err := c.Leases.Adopt(root, batch.Done(), inv.DiskBindingID, inv.RegistryID, release.Release); err != nil {
			return "", err
		}
	}
	locked = false
	runMu.Unlock()
	resp, err := c.Deliver(ctx, root, invocation)
	if err != nil {
		return "", err
	}
	if resp.IsError {
		return "", fmt.Errorf("batch delivery failed: %s", resp.Content)
	}
	return root, nil
}

func (c Mutations[I]) Add(ctx context.Context, caller, root, parent string, invocation I, reg *toolwrap.PolicyLeafRegistry, items []Item) ([]jobbatch.ID, error) {
	if c.Runner == nil || c.Access.store == nil {
		return nil, errors.New("batch runtime unavailable")
	}
	if caller == "" || caller != c.Invocation(invocation).SessionID || root == "" || len(items) == 0 {
		return nil, errors.New("batch_add requires caller, root and items")
	}
	inv, err := c.prepare(ctx, &invocation, func(inv MutationInvocation) bool {
		return slices.ContainsFunc(items, itemHasCustomRole) || inv.RoleContract != nil
	})
	if err != nil {
		return nil, err
	}
	s, err := c.Access.ReadAuthorized(ctx, caller, root)
	if err != nil {
		return nil, err
	}
	if s.Root.RootID != root || s.Root.Kind != "batch" || s.Root.HostID != c.Access.host || TerminalBatchStoreState(s.Root.State) {
		return nil, errors.New("batch_add requires live local owned root")
	}
	if c.Leases.Matches == nil || !c.Leases.Matches(jobbatch.ID(root), inv.DiskBindingID, inv.RegistryID) {
		return nil, c.DiskUnavailable
	}
	if parent == "" {
		parent = root
	}
	depths := map[string]int{root: 1}
	for progress := true; progress; {
		progress = false
		for _, n := range s.Nodes {
			if depths[n.NodeID] == 0 && n.ParentID.Valid {
				if d := depths[n.ParentID.String]; d > 0 {
					depths[n.NodeID] = d + 1
					progress = true
				}
			}
		}
	}
	depth := depths[parent]
	if depth == 0 {
		return nil, errors.New("batch_add parent not in root")
	}
	pn, ok := FindBatchNode(s.Nodes, parent)
	if !ok || pn.Kind != "batch" {
		return nil, errors.New("batch_add parent must be batch")
	}
	specs, err := BuildAddNodes(items, inv.AgentDepth, inv.CallID, func(raw Spec, depth int) (jobbatch.Spec, error) {
		return c.Build(raw, invocation, reg, depth)
	}, c.RewriteLeaf)
	if err != nil {
		return nil, err
	}
	for i := range specs {
		specs[i], err = c.AdmitRoles(inv.RoleConfig, inv.RoleContract, specs[i])
		if err != nil {
			return nil, err
		}
	}
	used := len(s.Nodes)
	for i := range specs {
		spec := specs[i]
		if err := assignModelBatchCallIDs(&spec, inv.CallID, fmt.Sprintf("add-%d", i), c.RewriteLeaf); err != nil {
			return nil, err
		}
		if err := c.ValidateAdd(inv.WaiterRank, jobbatch.ID(root), spec, depth, used); err != nil {
			return nil, err
		}
		_, added, err := c.Measure([]jobbatch.Spec{spec})
		if err != nil {
			return nil, err
		}
		used += added
		specs[i] = spec
	}
	return c.Runner.Add(ctx, jobbatch.AddRequest{Parent: jobbatch.ID(parent), Items: specs, Ref: inv.CallID})
}
