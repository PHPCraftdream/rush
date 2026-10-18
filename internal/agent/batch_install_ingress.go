package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/google/uuid"
)

// R11F (M3): the exported one-authority seam between the app-owned batch
// runtime and the coordinator's tool construction. Everything here is inert
// until BindCoordinatorBatchRuntime is called (flag-on wiring only); with no
// binding the construction code takes the old branch untouched.

// BatchLeafRegistry is the exported alias of the pinned policy-leaf registry
// (policy_leaf.go). Hosts never build one; they only pin resolved builds.
type BatchLeafRegistry = policyLeafRegistry

type BatchModelControl interface {
	batchModelControl
	InstallBatchModelControls(*BatchLeafRegistry, BatchInvocation) ([]fantasy.AgentTool, error)
}

func NewBatchModelControl(runner batchModelRunner, delivery batchDelivery, store *session.BatchStore, live *session.BatchLiveReader, sessions session.Service, host, runnerID string, profile *BatchProfile, resolver batchMCPCapabilityResolver, leases ...*batchRootLeaseStore) BatchModelControl {
	return NewInstalledBatchModelControl(runner, delivery, store, live, sessions, host, runnerID, profile, resolver, leases...)
}

// BatchWorkIngress is the ONE authority for work-tool invocation on the
// flag-on path: validate, attach-or-submit through the engine, deliver once.
type BatchWorkIngress interface {
	// Invoke is batchRuntime.Invoke (R11C ingress API).
	Invoke(ctx context.Context, inv BatchInvocation, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error)
	// PinLeafRegistry pins one construction's leaf registry under a fresh
	// identity (pinned-once: an identity is never replaced).
	PinLeafRegistry(registry *BatchLeafRegistry) (string, error)
	// ResolveLeaf resolves one pinned leaf by registry identity and name.
	ResolveLeaf(registryID, name string) (fantasy.AgentTool, error)
	// Registry returns the pinned registry of an identity (bridge wiring).
	Registry(registryID string) (*BatchLeafRegistry, error)
}

// batchWorkIngress adapts the unexported R11C runtime plus the M1 registry
// store onto the exported seam.
type batchWorkIngress struct {
	rt           *batchRuntime
	registries   *BatchLeafRegistryStore
	diskBindings *DiskProviderRegistry
	// caps is the same App-owned capability manager (typed alias); it owns
	// construction records and the owner set (R12-H2 §1).
	caps *diskCapabilityManager
}

var errBatchWorkIngressDeps = errors.New("batch work ingress: runtime and pinned registry store are required")

// NewBatchWorkIngress builds the exported ingress seam over one dormant
// runtime and its registry store. Construction has no side effects.
func NewBatchWorkIngress(rt *batchRuntime, registries *BatchLeafRegistryStore) (BatchWorkIngress, error) {
	if rt == nil || registries == nil || rt.cfg.DiskBindings == nil {
		return nil, errBatchWorkIngressDeps
	}
	return &batchWorkIngress{rt: rt, registries: registries, diskBindings: rt.cfg.DiskBindings, caps: rt.cfg.DiskBindings}, nil
}

// NewBatchWorkIngressWithDiskBindings injects the owning App's provider
// registry into one ingress/runtime; nil never selects a process global.
func NewBatchWorkIngressWithDiskBindings(rt *batchRuntime, registries *BatchLeafRegistryStore, bindings *DiskProviderRegistry) (BatchWorkIngress, error) {
	if rt == nil || registries == nil || bindings == nil {
		return nil, errBatchWorkIngressDeps
	}
	rt.cfg.DiskBindings = bindings
	return &batchWorkIngress{rt: rt, registries: registries, diskBindings: bindings, caps: bindings}, nil
}

func (b *batchWorkIngress) Invoke(ctx context.Context, inv BatchInvocation, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	ctx = withDiskProviderRegistry(ctx, b.diskBindings)
	return b.rt.Invoke(ctx, inv, leaf, call)
}

func (b *batchWorkIngress) PinLeafRegistry(registry *BatchLeafRegistry) (string, error) {
	if registry == nil {
		return "", errors.New("batch work ingress: nil leaf registry")
	}
	id := uuid.NewString()
	// Identity index registration only — deliberately NO ownership
	// operation here. Retention is expressed exclusively through typed
	// owner keys in the App capability manager: the caller (tool build or
	// private-fetch arming) takes the Construction owner atomically with
	// this pin and registers its release in the call/build scope; the
	// entry is deleted when no owner names the registry any more.
	if err := b.registries.Register(id, registry); err != nil {
		return "", err
	}
	return id, nil
}

func (b *batchWorkIngress) ResolveLeaf(registryID, name string) (fantasy.AgentTool, error) {
	registry, err := b.registries.Resolve(registryID)
	if err != nil {
		return nil, err
	}
	return registry.resolve(name)
}

func (b *batchWorkIngress) Registry(registryID string) (*BatchLeafRegistry, error) {
	return b.registries.Resolve(registryID)
}

func registryForIngress(ingress BatchWorkIngress) *DiskProviderRegistry {
	if b, ok := ingress.(*batchWorkIngress); ok {
		return b.diskBindings
	}
	return nil
}

// batchCoordinatorBinding is one coordinator's flag-on binding. R11F P1-4:
// it carries NO "latest pinned registry" — the CLI bridge binds to the
// TRUSTED ENCLOSING INVOCATION through a typed context value stamped by the
// agent-family driver (batchBridgeInvocation), so two concurrent invocations
// with different pinned registries can never cross-contaminate.
type batchCoordinatorBinding struct {
	ingress      BatchWorkIngress
	controls     BatchModelControl
	live         *session.BatchLiveReader
	diskBindings *DiskProviderRegistry
}

func (b *batchCoordinatorBinding) pin(registry *policyLeafRegistry) (string, error) {
	return b.ingress.PinLeafRegistry(registry)
}

var (
	// coordinatorBatchBindings maps a *coordinator onto its binding.
	coordinatorBatchBindings sync.Map
	// processBatchBinding is the process-wide binding (one install per
	// process, one Runner, one owner — the M2 one-authority decision).
	processBatchBinding atomic.Pointer[batchCoordinatorBinding]
)

// BindCoordinatorBatchRuntime binds one coordinator (and, first-wins, the
// process) to the flag-on batch runtime. After this returns, every new tool
// build re-arms its work tools through the ingress; the old async/stall
// wrappers are no longer installed. It fails closed on nil arguments.
func BindCoordinatorBatchRuntime(c Coordinator, ingress BatchWorkIngress) error {
	return BindCoordinatorBatchControls(c, ingress, nil)
}

func BindCoordinatorBatchControls(c Coordinator, ingress BatchWorkIngress, controls BatchModelControl, live ...*session.BatchLiveReader) error {
	return BindCoordinatorBatchRuntimeBindings(c, ingress, controls, registryForIngress(ingress), live...)
}

// BindCoordinatorBatchRuntimeBindings binds one coordinator to its specific
// runtime and provider-binding registry; no process-global provider lookup
// is used, so separate App instances remain isolated.
func BindCoordinatorBatchRuntimeBindings(c Coordinator, ingress BatchWorkIngress, controls BatchModelControl, diskBindings *DiskProviderRegistry, live ...*session.BatchLiveReader) error {
	if c == nil || ingress == nil || diskBindings == nil {
		return errors.New("batch coordinator binding: coordinator, ingress and disk registry are required")
	}
	bind := &batchCoordinatorBinding{ingress: ingress, controls: controls, diskBindings: diskBindings}
	if len(live) > 0 {
		bind.live = live[0]
	}
	coordinatorBatchBindings.Store(c, bind)
	processBatchBinding.CompareAndSwap(nil, bind)
	// MCP executor routing (R7a): names registered by the MCP family resolve
	// through the MCP kind; every other name keeps the built-in mapping.
	batchLeafKindOverride = func(tool string) (string, bool) {
		if strings.HasPrefix(tool, "mcp_") {
			return batchKindMCP, true
		}
		return "", false
	}
	cliprovider.SetBridgeFallback(coordinatorBridgeFallback)
	return nil
}

// coordinatorBatchBindingFor returns c's binding, or nil (flag-off).
func coordinatorBatchBindingFor(c *coordinator) *batchCoordinatorBinding {
	if c == nil {
		return nil
	}
	bind, ok := coordinatorBatchBindings.Load(c)
	if !ok {
		return nil
	}
	return bind.(*batchCoordinatorBinding)
}

// coordinatorPrivateFetchIngress returns the coordinator's OWN batch
// binding's ingress (the per-App explicit binding), or nil when the
// coordinator has no binding (flag-off). R12-H2a (P1-3): the agentic_fetch
// tool stamps THIS ingress — never the process-global binding — into the
// sub-agent build context so delegated private-fetch dispatch goes through
// the ONE per-App authority.
func coordinatorPrivateFetchIngress(c *coordinator) BatchWorkIngress {
	if bind := coordinatorBatchBindingFor(c); bind != nil {
		return bind.ingress
	}
	return nil
}

// processCoordinatorBatchBinding returns the process-wide binding or nil.
func processCoordinatorBatchBinding() *batchCoordinatorBinding {
	return processBatchBinding.Load()
}

// BindRuntimeIngress attaches the App-owned ingress to one install's
// runtime (R12-H2 P1-4) so every leaf env carries it and the agent-family
// driver resolves pinned leaves through the invocation's own authority
// instead of the process-global binding. Called from the app during
// install, before the runtime ever Invokes.
func BindRuntimeIngress(rt *batchRuntime, ingress BatchWorkIngress) {
	if rt == nil || ingress == nil {
		return
	}
	rt.setIngress(ingress)
}

// invokeBatchWithCallContext fills the invocation's per-call identity from
// the call-time context (session, message, model, image support) and hands
// it to the ONE authority. Missing identity fails closed inside Validate.
// A call option carrying a custom (non-OS) DiskProvider carries its
// IMMUTABLE binding handle (minted when the trusted call state was
// created); its opaque ID is stamped into the policy — the ID joins the
// canonical spec payload hash, so live attach with a different binding is
// rejected. A template pinning a binding already keeps it verbatim
// (nested/Add inheritance never re-resolves). A custom provider without a
// handle fails closed; nothing re-mints through a copied options object.
func invokeBatchWithCallContext(ctx context.Context, ingress BatchWorkIngress, tpl BatchInvocation, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	inv := tpl
	inv.SessionID = tools.GetSessionFromContext(ctx)
	inv.MessageID = tools.GetMessageFromContext(ctx)
	inv.CallID = call.ID
	inv.Policy.MessageID = inv.MessageID
	inv.Policy.SupportsImages = tools.GetSupportsImagesFromContext(ctx)
	inv.Policy.ModelName = tools.GetModelNameFromContext(ctx)
	if opts := callOptionsFrom(ctx); opts != nil {
		ctx = withDiskProviderRegistry(ctx, registryForIngress(ingress))
		if inv.Policy.DiskBindingID == "" && tools.IsCustomDiskProvider(opts.DiskProvider) {
			if opts.DiskBindingID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf(
					"%w: a custom disk provider must carry its binding handle before the call options are shared",
					ErrDiskProviderUnavailable)
			}
			inv.Policy.DiskBindingID = opts.DiskBindingID
		}
		inv.Policy.DiskProvider = inv.Policy.DiskBindingID != ""
	}
	if inv.Policy.DiskProvider && inv.Policy.DiskBindingID == "" {
		return fantasy.ToolResponse{}, ErrDiskProviderUnavailable
	}
	inv.Policy.CustomDisk = strings.TrimSpace(inv.Policy.CustomDisk)
	return ingress.Invoke(ctx, inv, leaf, call)
}

// batchPinnedInvoke closes the R7d pinnedInvoke over the ingress and a
// BatchInvocation template. Delegation target for the CLI bridge and the
// agentic_fetch private tools.
func batchPinnedInvoke(ingress BatchWorkIngress, tpl BatchInvocation) pinnedInvoke {
	return func(ctx context.Context, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return invokeBatchWithCallContext(ctx, ingress, tpl, leaf, call)
	}
}

// Exported aliases so the app package can wire the M1 install deps without
// naming unexported R7 types. Zero values keep the M1 fail-closed shapes.
type (
	BatchAgentDeps             = batchAgentDeps
	BatchAgentRunFactory       = batchAgentRunFactory
	BatchAgentDescendantReader = batchAgentDescendantReader
	BatchMCPCapability         = batchMCPCapability
	BatchMCPCapabilityResolver = batchMCPCapabilityResolver
)

// BatchNonReentrantMCPResolver is the flag-on default MCP admission
// resolver: every MCP tool is admitted as NON-reentrant (io group). A
// reentrant MCP tool therefore always fails closed
// (errBatchReentrantMCPUnannotated), never silently re-enters.
func BatchNonReentrantMCPResolver() BatchMCPCapabilityResolver {
	return func(string, string) (batchMCPCapability, bool) {
		return batchMCPCapability{}, true
	}
}
