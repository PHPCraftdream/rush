package agent

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/google/uuid"
)

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
//     is batch-admissible (batchIOTools), so its invocation enters the ONE
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
// Every category-(iii) tool above is covered by the batchIOTools /
// agent / agentic_fetch / mcp_ admission below; if a future WORK tool is
// added, it MUST join one of those families or it silently keeps a direct
// pinned leaf (this comment is the review anchor).
func batchWorkTarget(name string) bool {
	if batchControlTarget(name) {
		return false
	}
	if name == AgentToolName || name == tools.AgenticFetchToolName {
		return true
	}
	if batchIOTools[name] {
		return true
	}
	return strings.HasPrefix(name, "mcp_")
}

// batchFamilyDelivery is the per-family delivery choice of a batch-armed work
// tool: agent and agentic_fetch are DETACHED (a delegation returns the batch
// started text in-turn and the child runs detached -- the legacy async_tool
// contract; a synchronous wait would make the parent's tool call wait for a
// child that may itself be waiting for the parent's answer, or for a gate the
// parent turn opens); every other work family stays synchronous.
func batchFamilyDelivery(name string) batchDeliveryChoice {
	if name == AgentToolName || name == tools.AgenticFetchToolName {
		return batchDeliveryChoice{Mode: batchDeliveryDetached}
	}
	return batchDeliveryChoice{Mode: batchDeliverySync}
}

// batchIngressTool is the model-facing shell of one batch-armed work tool.
// Run resolves the pinned leaf (hooks/restricted wrappers retained,
// execution wrappers stripped) and enters the ONE authority.
type batchIngressTool struct {
	fantasy.AgentTool
	registry *policyLeafRegistry
	ingress  BatchWorkIngress
	inv      BatchInvocation
	// bind non-nil marks a DEFERRED build (shared default toolset): the
	// registry identity is minted and its Construction owner taken on the
	// FIRST dispatch, exactly once; every dispatch then holds its own
	// ephemeral Construction owner for the Invoke's duration, so the record
	// stays alive from before invokeFresh's Root owner (pre-Submit) until
	// the executor's Execution owner holds it, and dies by prune after the
	// root's Done + physical dispose (no process-lifetime fallback).
	bind          *batchCoordinatorBinding
	pctChildScope *pctChildToolScope // pct: exact captured task-agent build scope.
	once          sync.Once
	mu            sync.Mutex
	id            string
	perr          error
}

// ensurePinned publishes the deferred build's registry identity exactly
// once across concurrent dispatchers; the second dispatcher sees the
// published registry. NO Construction owner is taken here — ownership is
// per-dispatch (holdConstruction), so an unused-after-publish toolset
// still returns the manager to zero.
func (t *batchIngressTool) ensurePinned() (string, error) {
	t.once.Do(func() {
		id, err := t.bind.pin(t.registry)
		if err != nil {
			t.mu.Lock()
			t.perr = err
			t.mu.Unlock()
			return
		}
		t.mu.Lock()
		t.id = id
		t.mu.Unlock()
	})
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.id, t.perr
}

// holdConstruction takes ONE ephemeral Construction owner on the published
// identity for the caller's Invoke: the record is created on the first
// dispatch (acquireConstruction) and re-created if a prune dropped it
// between dispatches; every later dispatch holds an independent owner key.
// A prune that drops the record also deletes the registry-index entry; the
// dispatch itself repairs that mismatch through holdPinned, never here.
// The returned release MUST run after the Invoke returns — by then
// invokeFresh holds the Root owner (pre-Submit) and executor.Start the
// Execution owner, so the record dies by prune only after the root's Done
// + physical dispose.
func (t *batchIngressTool) holdConstruction(id string) (func(), error) {
	key := diskOwnerKey{kind: diskOwnerConstruction, instance: uuid.NewString(), registryID: id}
	if err := t.bind.diskBindings.acquire(key); err == nil {
		return func() { t.bind.diskBindings.release(key) }, nil
	}
	if err := t.bind.diskBindings.acquireConstruction("", id, key.instance); err != nil {
		return nil, err
	}
	return func() { t.bind.diskBindings.release(key) }, nil
}

// holdPinned returns a LIVE registry identity with one held Construction
// owner plus its release. It repairs the deferred-lifetime mismatch where
// pruneLocked deletes the identity-index entry together with the
// construction record while the deferred tool's pin-once identity stays
// minted (ensurePinned): a later dispatch re-created only the construction
// record (holdConstruction), so ResolveLeaf failed on the stale registry
// and the root went terminal without a Settled envelope. When the index no
// longer resolves id, the leaf registry is re-pinned under a fresh
// identity and THAT identity's construction is held instead. No
// process-wide retention: every re-pin dies by prune exactly like the
// first pin, and the capability-manager policy is untouched. The check is
// safe against concurrent prunes because the held Construction owner keeps
// the record alive while the index is read; a failed check therefore means
// the prune predates this dispatch.
func (t *batchIngressTool) holdPinned(id string) (string, func(), error) {
	t.mu.Lock()
	if t.id != "" {
		id = t.id
	}
	t.mu.Unlock()
	release, err := t.holdConstruction(id)
	if err != nil {
		return "", nil, err
	}
	if _, rerr := t.bind.ingress.Registry(id); rerr == nil {
		return id, release, nil
	}
	release()
	t.mu.Lock()
	fresh, perr := t.bind.pin(t.registry)
	if perr != nil {
		t.mu.Unlock()
		return "", nil, perr
	}
	t.id = fresh
	t.mu.Unlock()
	release, err = t.holdConstruction(fresh)
	if err != nil {
		return "", nil, err
	}
	return fresh, release, nil
}

func (t *batchIngressTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	// pct: clear inherited delegation state before ordinary work invocations.
	ctx = context.WithValue(ctx, pctDelegationHandoffKey{}, (*pctDelegationHandoff)(nil))
	if t.bind == nil {
		leaf, err := t.registry.resolve(call.Name)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		if t.pctChildScope != nil {
			lease := t.pctChildScope.pctRetain()
			if lease == nil {
				return fantasy.ToolResponse{}, ErrDiskProviderUnavailable
			}
			handoff := pctNewDelegationHandoff([]func(){lease})
			handoff.pctChild = t.pctChildScope
			if captured := pctCapturedToolFrom(leaf); captured == nil || !captured.pctRegister(pctInvocationKey(tools.GetSessionFromContext(ctx), call.ID), handoff) {
				handoff.pctRelease()
				return fantasy.ToolResponse{}, ErrDiskProviderUnavailable
			}
			defer handoff.pctReleasePending()
			ctx = context.WithValue(ctx, pctDelegationHandoffKey{}, handoff)
			return invokeBatchWithCallContext(ctx, t.ingress, t.inv, leaf, call)
		}
		return invokeBatchWithCallContext(ctx, t.ingress, t.inv, leaf, call)
	}
	// Deferred build: pin-on-first-dispatch, then hold a per-dispatch
	// Construction owner (independent instance IDs on the one identity)
	// until THIS Invoke returns — invokeFresh acquires the Root owner
	// before Submit and executor.Start the Execution owner, both naming
	// the registry, so the record survives the pin release only until the
	// root settles and physically disposes, then prunes to zero. A prune
	// between dispatches deletes the identity index entry while this tool
	// still holds the minted id; holdPinned re-pins in that case and the
	// invocation names the LIVE identity, never the stale one.
	id, err := t.ensurePinned()
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	leaf, err := t.registry.resolve(call.Name)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	inv := t.inv
	inv.RegistryID = id
	inv.Policy.RegistryID = id
	if inv.Policy.RestrictedToolset == "" {
		inv.Policy.RestrictedToolset = id
	}
	id, release, err := t.holdPinned(id)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	defer release()
	// holdPinned may have re-pinned after a prune: the invocation must
	// name the LIVE identity, never the stale one.
	inv.RegistryID = id
	inv.Policy.RegistryID = id
	return invokeBatchWithCallContext(ctx, t.ingress, inv, leaf, call)
}

// errBatchIngressTool fails closed: the ingress could not be armed (pin
// failure), so the tool refuses instead of falling back to the old path.
type errBatchIngressTool struct {
	fantasy.AgentTool
	err error
}

func (t errBatchIngressTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.ToolResponse{}, t.err
}

// newBatchWorkToolBuild pins the decorated slice and re-arms the work tools.
// Per-build state: ONE fresh pinned registry identity per construction; the
// per-invocation delivery choice follows the family: agent and agentic_fetch
// are DETACHED (escape never — a delegation returns the batch started text
// in-turn and the child runs detached, the legacy async_tool contract); bash
// keeps sync + escape auto + the unchanged inline window; every other work
// family stays sync with escape never.
//
// Construction ownership (R12-H2 §1.2): the pinned registry is published
// with a Construction owner key ATOMICALLY with the pin, and that owner's
// release is registered in the call/build scope of the trusted call state
// the build was made for. scope comes from the call's CallOptions; a nil
// scope fails closed (there is no "nil means process lifetime" fallback —
// ordinary, non-provider builds need the same closure semantics).
func newBatchWorkToolBuild(bind *batchCoordinatorBinding, list []fantasy.AgentTool, scope *buildResourceScope, deferred bool) (policyToolBuild, error) {
	if scope == nil {
		return policyToolBuild{}, errors.New("batch tool build: no build-resource scope in the trusted call state")
	}
	built := newPolicyToolBuild(list)
	fingerprint := batchToolsetFingerprint(list)
	if deferred {
		// Shared default toolset build (no trusted call state): the registry
		// identity is NOT published and NO Construction owner is taken here.
		// The armed tools pin lazily on first dispatch
		// (batchIngressTool.ensurePinned), so an unused shared toolset holds
		// nothing and a public run returns the capability manager exactly
		// to empty (design §1.2: bare builders create AND close a scope; no
		// process-lifetime fallback).
		for i, tool := range built.tools {
			name := tool.Info().Name
			if name == tools.AskQuestionToolName {
				built.tools[i] = batchAgentAskQuestionTool{AgentTool: tool}
				continue
			}
			if !batchWorkTarget(name) {
				continue
			}
			inv := BatchInvocation{
				Origin:           "coordinator",
				Delivery:         batchFamilyDelivery(name),
				BackgroundEscape: batchEscapeNever,
				WaiterRank:       0,
				Policy:           batchLeafPolicyContext{RestrictedToolset: fingerprint},
			}
			if name == tools.BashToolName {
				inv.BackgroundEscape = batchEscapeAuto
				inv.Delivery.Window = inlineWindow.String()
			}
			built.tools[i] = &batchIngressTool{AgentTool: tool, registry: built.leaves, ingress: bind.ingress, inv: inv, bind: bind}
		}
		return built, nil
	}
	registryID, err := bind.pin(built.leaves)
	if err != nil {
		return policyToolBuild{}, err
	}
	// Publish the construction record and take its Construction owner; the
	// owner's release lands in the call/build scope (or runs immediately
	// when the scope is already closed). Ordinary (non-provider) pinned
	// registries take the SAME ownership — no provider binding required.
	constructionKey := diskOwnerKey{kind: diskOwnerConstruction, instance: uuid.NewString(), registryID: registryID}
	if err := bind.diskBindings.acquireConstruction("", registryID, constructionKey.instance); err != nil {
		return policyToolBuild{}, err
	}
	scope.register(func() { bind.diskBindings.release(constructionKey) })
	built.registryID = registryID
	if fingerprint == "" {
		fingerprint = registryID
	}
	for i, tool := range built.tools {
		name := tool.Info().Name
		if name == tools.AskQuestionToolName {
			built.tools[i] = batchAgentAskQuestionTool{AgentTool: tool}
			continue
		}
		if !batchWorkTarget(name) {
			continue
		}
		inv := BatchInvocation{
			RegistryID:       registryID,
			Origin:           "coordinator",
			Delivery:         batchFamilyDelivery(name),
			BackgroundEscape: batchEscapeNever,
			WaiterRank:       0,
			Policy: batchLeafPolicyContext{
				RegistryID:        registryID,
				RestrictedToolset: fingerprint,
			},
		}
		if name == tools.BashToolName {
			// Auto-background keeps its window: escape switches DELIVERY of
			// the existing root after the window, never its execution.
			inv.BackgroundEscape = batchEscapeAuto
			inv.Delivery.Window = inlineWindow.String()
		}
		built.tools[i] = &batchIngressTool{AgentTool: tool, registry: built.leaves, ingress: bind.ingress, inv: inv, pctChildScope: pctCapturedScopeFromTool(tool)}
	}
	return built, nil
}

// batchToolsetFingerprint is the restricted-toolset identity of one build:
// the sorted tool names. Two builds with different toolsets carry different
// pinned registries AND different fingerprints.
func batchToolsetFingerprint(list []fantasy.AgentTool) string {
	names := make([]string, 0, len(list))
	for _, tool := range list {
		names = append(names, tool.Info().Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
