package agent

import (
	"context"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/installwire"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// batchInvocationSpec adapts the root-owned BatchInvocation template onto
// the toolwrap.Invocation port: immutable call-site identity ops. Every op
// returns a NEW view; the template value inside is never mutated (same
// semantics as the original by-value copies).
type batchInvocationSpec struct{ inv BatchInvocation }

func (s batchInvocationSpec) RegistryID() string { return s.inv.RegistryID }

func (s batchInvocationSpec) DispatchContext(ctx context.Context) context.Context {
	return withRoleToolContract(ctx, s.inv.Policy.RoleContract)
}

// ForIdentity names id on both registry identities and defaults an empty
// restricted toolset to id (the original deferred dispatch's first
// assignment block).
func (s batchInvocationSpec) ForIdentity(id string) toolwrap.Invocation {
	next := s.inv
	next.RegistryID = id
	next.Policy.RegistryID = id
	if next.Policy.RestrictedToolset == "" {
		next.Policy.RestrictedToolset = id
	}
	return batchInvocationSpec{next}
}

// ForLiveIdentity re-names ONLY the registry identities (the original
// post-holdPinned re-assignment; the restricted toolset is untouched).
func (s batchInvocationSpec) ForLiveIdentity(id string) toolwrap.Invocation {
	next := s.inv
	next.RegistryID = id
	next.Policy.RegistryID = id
	return batchInvocationSpec{next}
}

// batchIngressInvoke enters the ONE authority for the resolved leaf under
// the identity-rewritten invocation view (invokeBatchWithCallContext with
// the binding's ingress). The ingress is CAPTURED at construction — the
// original tool field was assigned from bind.ingress once at arming —
// while the RegistryKnown probe keeps its late lookup (the original read
// t.bind.ingress.Registry per dispatch). The typed assertion back onto
// batchInvocationSpec is safe: the ingress mints its views only through
// ForIdentity/ForLiveIdentity on the spec installed at construction.
func batchIngressInvoke(bind *batchCoordinatorBinding) func(context.Context, toolwrap.Invocation, fantasy.AgentTool, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	ingress := bind.ingress
	return func(ctx context.Context, inv toolwrap.Invocation, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return invokeBatchWithCallContext(ctx, ingress, inv.(batchInvocationSpec).inv, leaf, call)
	}
}

// diskConstructionOwners translates the ingress's (registryID, instance)
// owner pair onto the root capability manager's typed owner key. It holds
// the BINDING, not the manager pointer: the original closures read
// t.bind.diskBindings at every acquire/release execution, so a re-bound
// manager stays visible to already-armed tools. The key shape (kind
// diskOwnerConstruction) and therefore liveness/prune timing are identical
// to the original holdConstruction key construction.
type diskConstructionOwners struct {
	bind *batchCoordinatorBinding
}

func (o diskConstructionOwners) Acquire(registryID, instance string) error {
	return o.bind.diskBindings.acquire(diskOwnerKey{kind: diskOwnerConstruction, instance: instance, registryID: registryID})
}

func (o diskConstructionOwners) AcquireConstruction(registryID, instance string) error {
	return o.bind.diskBindings.acquireConstruction("", registryID, instance)
}

func (o diskConstructionOwners) Release(registryID, instance string) {
	o.bind.diskBindings.release(diskOwnerKey{kind: diskOwnerConstruction, instance: instance, registryID: registryID})
}

// newBatchIngressShell arms one work tool through the moved ingress shell,
// passing the root-owned pin, registry and acquisition/release through the
// narrow DeferredBuild/Invocation ports. deferred selects the deferred
// (shared default toolset) binding shape; the pinned shape carries the
// captured task-agent child scope instead.
func newBatchIngressShell(tool fantasy.AgentTool, leaves *policyLeafRegistry, bind *batchCoordinatorBinding, inv BatchInvocation, deferred bool) *toolwrap.BatchIngressTool {
	shell := &toolwrap.BatchIngressTool{
		AgentTool: tool,
		Registry:  leaves,
		Inv:       batchInvocationSpec{inv},
		Invoke:    batchIngressInvoke(bind),
		Captured: func(leaf fantasy.AgentTool) toolwrap.CapturedRegistrar {
			// typed-nil normalization: a nil *pctCapturedAgentTool must not
			// arm the registrar interface (the ingress checks nil to pick
			// the no-captured path).
			if captured := childagent.CapturedAgentToolFrom(leaf); captured != nil {
				return captured
			}
			return nil
		},
		DiskUnavailable: func() error { return ErrDiskProviderUnavailable },
	}
	if deferred {
		shell.Bind = &toolwrap.DeferredBuild{
			Pin:           func() (string, error) { return bind.pin(leaves) },
			RegistryKnown: func(id string) bool { _, err := bind.ingress.Registry(id); return err == nil },
			Owners:        diskConstructionOwners{bind},
		}
		return shell
	}
	shell.PctChildScope = toolwrap.CapturedScopeFromTool(tool)
	return shell
}

// R11F (M3): flag-on tool assembly. The decorated tool slice is pinned into
// a fresh leaf registry and every batch-admissible WORK tool is re-armed so
// its invocation enters the ONE authority (batchRuntime.Invoke). The old
// async/stall wrappers are NOT installed on this path; hooks, restricted-run
// gating and permissions run exactly once — inside the pinned leaf the
// executor resolves.

// batchWorkTarget reports whether a constructed tool is a batch-admissible
// work leaf: the D1 complement (never a control-plane tool) restricted to
// the families the R11A admission validator can classify. Tools outside
// every family (todos, rush_info, ...) keep their direct pinned
// leaf — forcing them through the ingress would only convert every call
// into errBatchUnknownTool.
// Completeness inventory (R11F round-2), per non-wrapped tool of the D1
// complement:
//
//   - todos: (iii) WORK — it MUTATES session state (writes the session's
//     todo list that the worker turns read back). It IS wrapped below: it
//     is batch-admissible (batchinst.IsIOTool), so its invocation enters the ONE
//     authority like any other state-changing leaf. The admission validator
//     classifies it in the io group.
//   - batch_status/batch_output/batch_stop: (i) D1 CONTROLS —
//     control-plane (batchControlTarget/BatchNonWorkTarget), never
//     batch-wrapped.
//   - rush_info, rush_logs: (ii) NOT WORK — pure read-only host/process
//     introspection with no external effect and no session-state mutation;
//     direct pinned leaf.
//   - read_delegation_transcript: (ii) NOT WORK — read-only transcript
//     inspection; direct pinned leaf.
//   - list_mcp_resources, read_mcp_resource: (ii) NOT WORK — read-only MCP
//     resource listing/reading (no execution, no external mutation); direct
//     pinned leaf.
//   - ask-adjacent (ask_question and every wake/loop/inspect/inject
//     control): (i) D1 control (batchControlTarget) — never wrapped.
//
// Every category-(iii) tool above is covered by the batchinst.IsIOTool /
// agent / agentic_fetch / mcp_ admission below; if a future WORK tool is
// added, it MUST join one of those families or it silently keeps a direct
// pinned leaf (this comment is the review anchor).
func batchWorkTarget(name string) bool {
	return installwire.WorkTarget(name, batchControlTarget, batchinst.IsIOTool)
}

// batchFamilyDelivery is the per-family delivery choice of a batch-armed work
// tool: agent and agentic_fetch are DETACHED (a delegation returns the batch
// started text in-turn and the child runs detached -- the legacy async_tool
// contract; a synchronous wait would make the parent's tool call wait for a
// child that may itself be waiting for the parent's answer, or for a gate the
// parent turn opens); every other work family stays synchronous.
func batchFamilyDelivery(name string) batchDeliveryChoice {
	return installwire.FamilyDelivery(name, batchDeliveryChoice{Mode: batchDeliveryDetached}, batchDeliveryChoice{Mode: batchDeliverySync})
}

// errBatchIngressTool fails closed: the ingress could not be armed (pin
// failure), so the tool refuses instead of falling back to the old path.
type errBatchIngressTool struct {
	fantasy.AgentTool
	err error
}

func (t errBatchIngressTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return installwire.NewRefusedTool(t.AgentTool, t.err).Run(ctx, call)
}

// newBatchWorkToolBuild uses the default context for shared tool assembly.
func newBatchWorkToolBuild(bind *batchCoordinatorBinding, list []fantasy.AgentTool, scope *buildResourceScope, deferred bool) (policyToolBuild, error) {
	return newBatchWorkToolBuildForContext(context.Background(), bind, list, scope, deferred)
}

func newBatchWorkToolBuildForContext(ctx context.Context, bind *batchCoordinatorBinding, list []fantasy.AgentTool, scope *buildResourceScope, deferred bool) (policyToolBuild, error) {
	var registerRelease func(func())
	if scope != nil {
		registerRelease = scope.Register
	}
	return installwire.BuildWorkTools(ctx, list, registerRelease, deferred, installwire.WorkToolPorts[*session.RoleContract]{
		Pin:     bind.pin,
		Owners:  diskConstructionOwners{bind},
		Role:    roleContractForToolBuild,
		Allows:  roleContractAllowsName,
		Control: batchControlTarget,
		IO:      batchinst.IsIOTool,
		WrapQuestion: func(tool fantasy.AgentTool) fantasy.AgentTool {
			return batchAgentAskQuestionTool{AgentTool: tool}
		},
		NoWindow: func() bool { return inlineWindowDisabledFor(bind) },
		Arm: func(tool fantasy.AgentTool, leaves *policyLeafRegistry, role *session.RoleContract, template installwire.WorkToolTemplate, deferred bool) fantasy.AgentTool {
			inv := BatchInvocation{
				RegistryID:       template.RegistryID,
				Origin:           "coordinator",
				Delivery:         batchDeliveryChoice{Mode: batchDeliverySync},
				BackgroundEscape: batchEscapeNever,
				Policy: batchLeafPolicyContext{
					RegistryID:        template.RegistryID,
					RestrictedToolset: template.RestrictedToolset,
					RoleContract:      cloneRoleContractForCall(role),
				},
			}
			if template.Detached {
				inv.Delivery.Mode = batchDeliveryDetached
			}
			if template.AutoEscape {
				inv.BackgroundEscape = batchEscapeAuto
				inv.Delivery.Window = installwire.InlineWindow.String()
			}
			return newBatchIngressShell(tool, leaves, bind, inv, deferred)
		},
	})
}
