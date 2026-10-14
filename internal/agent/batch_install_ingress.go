package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/google/uuid"
)

// R11F (M3): the exported one-authority seam between the app-owned batch
// runtime and the coordinator's tool construction. Everything here is inert
// until BindCoordinatorBatchRuntime is called (flag-on wiring only); with no
// binding the construction code takes the old branch untouched.

// BatchLeafRegistry is the exported alias of the pinned policy-leaf registry
// (policy_leaf.go). Hosts never build one; they only pin resolved builds.
type BatchLeafRegistry = policyLeafRegistry

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
	rt         *batchRuntime
	registries *BatchLeafRegistryStore
}

var errBatchWorkIngressDeps = errors.New("batch work ingress: runtime and pinned registry store are required")

// NewBatchWorkIngress builds the exported ingress seam over one dormant
// runtime and its registry store. Construction has no side effects.
func NewBatchWorkIngress(rt *batchRuntime, registries *BatchLeafRegistryStore) (BatchWorkIngress, error) {
	if rt == nil || registries == nil {
		return nil, errBatchWorkIngressDeps
	}
	return &batchWorkIngress{rt: rt, registries: registries}, nil
}

func (b *batchWorkIngress) Invoke(ctx context.Context, inv BatchInvocation, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return b.rt.Invoke(ctx, inv, leaf, call)
}

func (b *batchWorkIngress) PinLeafRegistry(registry *BatchLeafRegistry) (string, error) {
	if registry == nil {
		return "", errors.New("batch work ingress: nil leaf registry")
	}
	id := uuid.NewString()
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

// batchCoordinatorBinding is one coordinator's flag-on binding. R11F P1-4:
// it carries NO "latest pinned registry" — the CLI bridge binds to the
// TRUSTED ENCLOSING INVOCATION through a typed context value stamped by the
// agent-family driver (batchBridgeInvocation), so two concurrent invocations
// with different pinned registries can never cross-contaminate.
type batchCoordinatorBinding struct {
	ingress BatchWorkIngress
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
	if c == nil || ingress == nil {
		return errors.New("batch coordinator binding: coordinator and ingress are required")
	}
	bind := &batchCoordinatorBinding{ingress: ingress}
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

// processCoordinatorBatchBinding returns the process-wide binding or nil.
func processCoordinatorBatchBinding() *batchCoordinatorBinding {
	return processBatchBinding.Load()
}

// invokeBatchWithCallContext fills the invocation's per-call identity from
// the call-time context (session, message, model, image support) and hands
// it to the ONE authority. Missing identity fails closed inside Validate.
func invokeBatchWithCallContext(ctx context.Context, ingress BatchWorkIngress, tpl BatchInvocation, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	inv := tpl
	inv.SessionID = tools.GetSessionFromContext(ctx)
	inv.MessageID = tools.GetMessageFromContext(ctx)
	inv.Policy.MessageID = inv.MessageID
	inv.Policy.SupportsImages = tools.GetSupportsImagesFromContext(ctx)
	inv.Policy.ModelName = tools.GetModelNameFromContext(ctx)
	if opts := callOptionsFrom(ctx); opts != nil {
		inv.Policy.DiskProvider = inv.Policy.DiskProvider || tools.HasDiskProvider(opts.DiskProvider)
	}
	inv.Policy.CustomDisk = strings.TrimSpace(inv.Policy.CustomDisk)
	if inv.Policy.DiskProvider || inv.Policy.CustomDisk != "" {
		return fantasy.NewTextErrorResponse(ErrDiskProviderNotDurable.Error()), nil
	}
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
