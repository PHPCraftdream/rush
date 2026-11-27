package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchadapter"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore/modelcontrol"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent/state"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/installwire"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
)

// R11F (M3): the exported one-authority seam between the app-owned batch
// runtime and the coordinator's tool construction. Everything here is inert
// until BindCoordinatorBatchRuntime is called (flag-on wiring only); with no
// binding the construction code takes the old branch untouched.

type BatchModelControl = installwire.ModelControl[BatchInvocation, modelcontrol.Spec, modelcontrol.Item]

func NewBatchModelControl(runner batchModelRunner, delivery batchDelivery, store *session.BatchStore, live *session.BatchLiveReader, sessions coresession.Service, host, runnerID string, profile *BatchProfile, resolver batchinst.MCPCapabilityResolver, leases ...*batchRootLeaseStore) BatchModelControl {
	return NewInstalledBatchModelControl(runner, delivery, store, live, sessions, host, runnerID, profile, resolver, leases...)
}

// BatchWorkIngress is the ONE authority for work-tool invocation on the
// flag-on path: validate, attach-or-submit through the engine, deliver once.
type BatchWorkIngress interface {
	// Invoke is batchRuntime.Invoke (R11C ingress API).
	Invoke(ctx context.Context, inv BatchInvocation, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error)
	// PinLeafRegistry pins one construction's leaf registry under a fresh
	// identity (pinned-once: an identity is never replaced).
	PinLeafRegistry(registry *toolwrap.PolicyLeafRegistry) (string, error)
	// ResolveLeaf resolves one pinned leaf by registry identity and name.
	ResolveLeaf(registryID, name string) (fantasy.AgentTool, error)
	// Registry returns the pinned registry of an identity (bridge wiring).
	Registry(registryID string) (*toolwrap.PolicyLeafRegistry, error)
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

// NewBatchWorkIngress builds the exported ingress over a dormant runtime.
func NewBatchWorkIngress(rt *batchRuntime, registries *BatchLeafRegistryStore) (BatchWorkIngress, error) {
	var bindings *DiskProviderRegistry
	if rt != nil {
		bindings = rt.cfg.DiskBindings
	}
	return newBatchWorkIngress(rt, registries, bindings, nil)
}

// NewBatchWorkIngressWithDiskBindings injects the owning App's registry.
func NewBatchWorkIngressWithDiskBindings(rt *batchRuntime, registries *BatchLeafRegistryStore, bindings *DiskProviderRegistry) (BatchWorkIngress, error) {
	return newBatchWorkIngress(rt, registries, bindings, func(b *DiskProviderRegistry) { rt.cfg.DiskBindings = b })
}

func newBatchWorkIngress(rt *batchRuntime, registries *BatchLeafRegistryStore, bindings *DiskProviderRegistry, configure func(*DiskProviderRegistry)) (BatchWorkIngress, error) {
	var run installwire.Invoke[BatchInvocation]
	if rt != nil {
		run = rt.Invoke
	}
	var index installwire.RegistryIndex
	if registries != nil {
		index = registries
	}
	wire, err := installwire.NewIngress(run, index, bindings, configure, withDiskProviderRegistry)
	if err != nil {
		return nil, err
	}
	return &batchWorkIngress{rt: rt, registries: registries, diskBindings: wire.DiskBindings, caps: wire.DiskBindings}, nil
}

// Keep the root fixture's fields as the source of truth, including for
// test-only ingress values built without a runtime.
func (b *batchWorkIngress) wire() installwire.Ingress[BatchInvocation, *DiskProviderRegistry] {
	return installwire.Ingress[BatchInvocation, *DiskProviderRegistry]{Run: b.rt.Invoke, Registries: b.registries, DiskBindings: b.diskBindings, WithDiskBindings: withDiskProviderRegistry}
}

func (b *batchWorkIngress) Invoke(ctx context.Context, inv BatchInvocation, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return b.wire().Invoke(ctx, inv, leaf, call)
}

func (b *batchWorkIngress) PinLeafRegistry(registry *toolwrap.PolicyLeafRegistry) (string, error) {
	return b.wire().PinLeafRegistry(registry)
}

func (b *batchWorkIngress) ResolveLeaf(registryID, name string) (fantasy.AgentTool, error) {
	return b.wire().ResolveLeaf(registryID, name)
}

func (b *batchWorkIngress) Registry(registryID string) (*toolwrap.PolicyLeafRegistry, error) {
	return b.wire().Registry(registryID)
}

func (b *batchWorkIngress) InstallDiskBindings() *DiskProviderRegistry { return b.diskBindings }

func registryForIngress(ingress BatchWorkIngress) *DiskProviderRegistry {
	return installwire.RegistryForIngress[*DiskProviderRegistry, *batchWorkIngress](ingress)
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
	// coord is the coordinator this binding was created for; nil for a
	// binding built without one (test fixtures). It carries ONLY the
	// test-visible delivery window (inlineWindowDisabled) -- never runtime
	// authority, which lives in ingress/controls.
	coord *coordinator
}

func (b *batchCoordinatorBinding) pin(registry *toolwrap.PolicyLeafRegistry) (string, error) {
	return b.ingress.PinLeafRegistry(registry)
}

type batchCoordinatorBindingIndex interface {
	Store(Coordinator, *batchCoordinatorBinding)
	Load(Coordinator) (*batchCoordinatorBinding, bool)
}

var (
	installedBatchBindings   installwire.Bindings[Coordinator, batchCoordinatorBinding]
	coordinatorBatchBindings batchCoordinatorBindingIndex = &installedBatchBindings
	processBatchBinding                                   = installedBatchBindings.ProcessSlot()
)

// BindCoordinatorBatchRuntime binds one coordinator (and, first-wins, the
// process) to the flag-on batch runtime. After this returns, every new tool
// build re-arms its work tools through the ingress; the old async/stall
// wrappers are no longer installed. It fails closed on nil arguments.
func BindCoordinatorBatchRuntime(c Coordinator, ingress BatchWorkIngress) error {
	return installwire.BindCoordinatorRuntime(c, ingress, func(c Coordinator, ingress BatchWorkIngress) error {
		return BindCoordinatorBatchControls(c, ingress, nil)
	})
}

func BindCoordinatorBatchControls(c Coordinator, ingress BatchWorkIngress, controls BatchModelControl, live ...*session.BatchLiveReader) error {
	return BindCoordinatorBatchRuntimeBindings(c, ingress, controls, registryForIngress(ingress), live...)
}

// coordinatorBatchBindingFor returns c's binding, or nil (flag-off).
func coordinatorBatchBindingFor(c *coordinator) *batchCoordinatorBinding {
	if c == nil {
		return nil
	}
	bind, _ := coordinatorBatchBindings.Load(c)
	return bind
}

// coordinatorPrivateFetchIngress returns the coordinator's OWN batch
// binding's ingress (the per-App explicit binding), or nil when the
// coordinator has no binding (flag-off). R12-H2a (P1-3): the agentic_fetch
// tool stamps THIS ingress — never the process-global binding — into the
// sub-agent build context so delegated private-fetch dispatch goes through
// the ONE per-App authority.
func coordinatorPrivateFetchIngress(c *coordinator) BatchWorkIngress {
	return installwire.CoordinatorIngress(coordinatorBatchBindingFor(c), func(bind *batchCoordinatorBinding) BatchWorkIngress { return bind.ingress })
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
	installwire.BindRuntimeIngress(rt.setIngress, ingress)
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
	if !inv.AgentDepthSet {
		inv.AgentDepth = DefaultAgentDepth
		inv.AgentDepthSet = true
	}
	if opts := callOptionsFrom(ctx); opts != nil && opts.AgentDepthSet && opts.MaxAgentDepth < inv.AgentDepth {
		inv.AgentDepth = opts.MaxAgentDepth
	}
	if inv.AgentDepth < 0 || inv.AgentDepth > 32 {
		return fantasy.ToolResponse{}, errors.New("agent depth must be between 0 and 32")
	}
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
	// The calling session's own answer route — the same rule as the model
	// control (modelInvocationAtCall): an absent marker is a top-level
	// session whose own surface is the established route; a present marker
	// is a batch incarnation inheriting its durable payload value. This
	// adapter is the ordinary direct-call path, so without the stamp a
	// parent-owned `agent` delegation would silently lose the route and a
	// no-route child could mint route-ful leaves.
	inv.QuestionRoute = true
	if route, ok := batchmeta.BatchQuestionRouteFrom(ctx); ok {
		inv.QuestionRoute = route
	}
	return ingress.Invoke(ctx, inv, leaf, call)
}

// batchPinnedInvoke closes the R7d childagent.PinnedInvoke over the ingress and a
// BatchInvocation template. Delegation target for the CLI bridge and the
// agentic_fetch private tools.
func batchPinnedInvoke(ingress BatchWorkIngress, tpl BatchInvocation) childagent.PinnedInvoke {
	return installwire.PinnedInvoke(ingress, tpl, invokeBatchWithCallContext)
}

// Exported aliases so the app package can wire the M1 install deps without
// naming unexported R7 types. Zero values keep the M1 fail-closed shapes.
type (
	BatchAgentDeps             = batchAgentDeps
	BatchAgentRunFactory       = batchAgentRunFactory
	BatchAgentDescendantReader = batchAgentDescendantReader
)

// BatchNonReentrantMCPResolver is the flag-on default MCP admission
// resolver: every MCP tool is admitted as NON-reentrant (io group). A
// reentrant MCP tool therefore always fails closed
// (errBatchReentrantMCPUnannotated), never silently re-enters.
func BatchNonReentrantMCPResolver() installwire.MCPCapabilityResolver {
	return installwire.NonReentrantMCPResolver()
}

// BatchLeafRegistryStore is the pinned R1 registry resolver keyed by the
// invocation's registry identity. Registration is pinned-once: an identity
// can never be replaced, so a live call's leaf set cannot change under it.
// Lookup fails closed on an unknown identity.
type BatchLeafRegistryStore struct {
	store installwire.RegistryStore[batchCredentialEscrow]
}

func (s *BatchLeafRegistryStore) credentialEscrow() *batchCredentialEscrow {
	return s.store.CredentialEscrow(newBatchCredentialEscrow)
}

func NewBatchLeafRegistryStore() *BatchLeafRegistryStore {
	return &BatchLeafRegistryStore{store: installwire.NewRegistryStore(newBatchCredentialEscrow)}
}

func (s *BatchLeafRegistryStore) Register(registryID string, registry *toolwrap.PolicyLeafRegistry) error {
	return s.store.Register(registryID, registry, newBatchCredentialEscrow, func(tool fantasy.AgentTool, escrow *batchCredentialEscrow) fantasy.AgentTool {
		return &batchCredentialLeaf{AgentTool: tool, escrow: escrow}
	})
}

func (s *BatchLeafRegistryStore) Resolve(id string) (*toolwrap.PolicyLeafRegistry, error) {
	return s.store.Resolve(id)
}

func (s *BatchLeafRegistryStore) Delete(id string) { s.store.Delete(id) }
func (s *BatchLeafRegistryStore) Count() int       { return s.store.Count() }

// R11F (M3): coordinator seams of the R7c agent executor family. The M2
// install left batchAgentDeps zero; the flagged install wires
// CoordinatorBatchAgentDeps, whose factory resolves the ENCLOSED
// invocation's pinned "agent" leaf (hooks/restricted retained) through the
// one ingress and runs it as the incarnation's turn loop.

// errBatchCoordinatorNotBound: the flag-on wiring (BindCoordinatorBatchRuntime)
// has not run — the agent family refuses instead of dispatching raw.
var errBatchCoordinatorNotBound = errors.New("batch agent family: no coordinator batch binding (flag-on wiring missing)")

// batchCoordinatorAgentRuns is the R7c run factory over the process binding.
type batchCoordinatorAgentRuns struct{}

// StartAgentTurn launches one incarnation: the pinned delegation leaf runs
// to completion on its own goroutine. R11F round-2 (P1-3): dispatch is by
// the RECORDED tool name (req.Tool) — an "agentic_fetch" payload resolves
// its own pinned leaf and names the call agentic_fetch; an empty name fails
// closed.
func (batchCoordinatorAgentRuns) StartAgentTurn(ctx context.Context, req batchAgentRunRequest, host childagent.Host) (childagent.Turn, error) {
	// R12-H2 P1-4: resolve the pinned leaf through the App-owned ingress
	// carried on the request (the invocation's own authority). The
	// process-global binding is only a fallback for flag-off-adjacent
	// fixtures — NEVER for provider-bound work: a bound incarnation with no
	// request ingress fails closed instead of touching process state.
	ingress := req.Ingress
	if ingress == nil {
		if req.DiskBindingID != "" {
			return nil, ErrDiskProviderUnavailable
		}
		if bind := installwire.ProcessBinding(processBatchBinding); bind != nil {
			ingress = bind.ingress
		}
	}
	if ingress == nil {
		return nil, errBatchCoordinatorNotBound
	}
	if req.RegistryID == "" {
		return nil, fmt.Errorf("%w: batch agent incarnation", errBatchMissingRegistryID)
	}
	if req.Tool == "" {
		// P1-3: fail closed rather than silently dispatching "agent".
		return nil, fmt.Errorf("%w: batch agent incarnation without a recorded tool name", batchinst.ErrBatchUnknownTool)
	}
	leaf, err := ingress.ResolveLeaf(req.RegistryID, req.Tool)
	if err != nil {
		return nil, err
	}
	if req.CallCredentials {
		credentialLeaf, ok := leaf.(interface {
			credentialEscrow() *batchCredentialEscrow
		})
		if !ok {
			return nil, batchadapter.ErrCredentialEscrowMissing
		}
		creds, releaseCredentials, err := credentialLeaf.credentialEscrow().consume(batchCredentialKey{
			registryID: req.RegistryID, sessionID: req.SessionID, messageID: req.MessageID,
			callID: req.ToolCallID, tool: req.Tool,
		}, string(req.NodeID))
		if err != nil {
			return nil, err
		}
		return startBatchAgentTurn(ctx, req, host, leaf, creds, releaseCredentials)
	}
	return startBatchAgentTurn(ctx, req, host, leaf, nil, func() {})
}

func startBatchAgentTurn(ctx context.Context, req batchAgentRunRequest, host childagent.Host, leaf fantasy.AgentTool, creds *CredentialSet, releaseCredentials func()) (childagent.Turn, error) {
	if req.DiskBindingID != "" && batchdispatch.DiskProviderFrom(ctx) == nil {
		releaseCredentials()
		return nil, ErrDiskProviderUnavailable
	}
	if req.AgentDepth < 0 || req.AgentDepth > 32 {
		return nil, errors.New("batch agent depth must be between 0 and 32")
	}
	depth := state.QuestionDepth(ctx) + 1
	if depth <= 0 {
		depth = 1
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if req.Policy != nil && req.Policy.RoleContract != nil {
		runCtx = WithCallRoleContract(runCtx, req.Policy.RoleContract)
	}
	if req.SelectedRoleContract != nil {
		runCtx = WithSelectedRoleContract(runCtx, req.SelectedRoleContract)
	}
	if req.CallCredentials {
		runCtx = withCallCredentials(runCtx, creds)
	}
	runCtx = state.WithQuestionContext(runCtx, depth)
	// The question marker travels on EVERY batch incarnation's run context
	// (both values), so the per-call child toolset shaping can tell a
	// no-route incarnation from a non-batch one. The held-answer bridge
	// itself is armed ONLY for route-ful incarnations: an unattended
	// batch child never gets a working-looking ask.
	runCtx = batchmeta.WithBatchQuestionRoute(runCtx, req.QuestionRoute)
	if req.QuestionRoute {
		runCtx = withBatchAgentQuestionBinding(runCtx, batchAgentQuestionBinding{Ask: host.Ask})
	}
	tracked := childagent.TrackChild(host, req.ChildSessionID != "")
	lifecycle := state.Lifecycle{NoChildSelected: tracked.NoChildSelected, BindChildSession: tracked.BindChildSession}
	runCtx = state.WithLifecycle(runCtx, lifecycle)
	runCtx = context.WithValue(runCtx, tools.SessionIDContextKey, req.SessionID)
	runCtx = context.WithValue(runCtx, tools.MessageIDContextKey, req.MessageID)
	// P1-4: stamp the TRUSTED enclosing invocation so the CLI bridge binds
	// to THIS invocation's pinned registry (never the latest one).
	runCtx = state.WithBridgeInvocation(runCtx, state.BridgeInvocation{RegistryID: req.RegistryID, MessageID: req.MessageID, AgentDepth: req.AgentDepth, AgentDepthSet: true})
	callOptions, err := WithAgentDepth(runCtx, req.AgentDepth)
	if err != nil {
		cancel()
		releaseCredentials()
		return nil, err
	}
	runCtx = callOptions
	var childScope *buildResourceScope
	var provider tools.DiskProvider
	if req.DiskBindingID != "" {
		provider = batchdispatch.DiskProviderFrom(ctx)
		if provider == nil {
			cancel()
			releaseCredentials()
			return nil, ErrDiskProviderUnavailable
		}
		co := boundCallOptions(provider, req.DiskBindingID, batchDiskRegistryFromContext(ctx))
		co.MaxAgentDepth = req.AgentDepth
		co.AgentDepthSet = true
		co.BuildScope = newBuildResourceScope()
		childScope = co.BuildScope
		runCtx = WithCallOptions(runCtx, co)
	}
	call := fantasy.ToolCall{ID: req.ToolCallID, Name: req.Tool, Input: string(req.Input)}
	return childagent.RunTool(runCtx, cancel, leaf, call, lifecycle.NoChildSelected, childScope.Close, releaseCredentials), nil
}

// CoordinatorBatchAgentDeps builds the flag-on agent-family seams for the
// M1 install (BatchInstallDeps.AgentDeps). Before the process binding runs
// the family fails closed with errBatchCoordinatorNotBound.
// CoordinatorBatchAgentDeps builds the flag-on agent-family seams WITHOUT
// the store-backed descendant reader: DescendantsLive then reports an
// INCOMPLETE read (false, false) — the scope stays open, never an optimistic
// closure. Flagged installs use CoordinatorBatchAgentDepsWithLive (P1-5b).
func CoordinatorBatchAgentDeps() BatchAgentDeps {
	return batchAgentDeps{
		Runs:        batchCoordinatorAgentRuns{},
		Descendants: batchCoordinatorDescendants{},
	}
}
