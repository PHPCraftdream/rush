// Package toolwrap — batch ingress (DM4 chunk 4, moved verbatim from package
// agent's batch_install_tools.go and agentic_fetch_policy.go). The ingress
// tool shells and the private fetch ingress shell live here; the concrete
// BatchInvocation template, the ONE authority (invokeBatchWithCallContext)
// and the coordinator binding stay in package agent and are reached only
// through the narrow typed ports below (Invocation, DeferredBuild,
// constructionOwners) plus the Invoke/Captured function fields — this
// package never imports package agent.
package toolwrap

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/agenticfetch"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/google/uuid"
)

var agenticFetchToolDescription = agenticfetch.ToolDescription()

// agenticFetchValidationResult holds the validated parameters from the tool call context.
type agenticFetchValidationResult struct {
	SessionID      string
	AgentMessageID string
}

// ValidateAgenticFetchParams validates the tool call parameters and extracts required context values.
func ValidateAgenticFetchParams(ctx context.Context, params tools.AgenticFetchParams) (agenticFetchValidationResult, error) {
	if params.Prompt == "" {
		return agenticFetchValidationResult{}, errors.New("prompt is required")
	}

	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return agenticFetchValidationResult{}, errors.New("session id missing from context")
	}

	agentMessageID := tools.GetMessageFromContext(ctx)
	if agentMessageID == "" {
		return agenticFetchValidationResult{}, errors.New("agent message id missing from context")
	}

	return agenticFetchValidationResult{
		SessionID:      sessionID,
		AgentMessageID: agentMessageID,
	}, nil
}

var agenticFetchPromptTmpl = agenticfetch.PromptTemplate()

// AgenticFetchToolDescription returns the agentic_fetch tool description
// (embedded template asset owned by the agenticfetch package). A getter,
// not a mutable export.
func AgenticFetchToolDescription() string { return agenticFetchToolDescription }

// AgenticFetchPromptTmpl returns the agentic_fetch prompt template. A
// getter, not a mutable export.
func AgenticFetchPromptTmpl() []byte { return agenticFetchPromptTmpl }

// Invocation is the typed, immutable call-site port over the root-owned
// BatchInvocation template (package agent): the identity rewrite ops the
// deferred dispatch performs. Root-side delivery assertions read the
// original typed template directly, not this port. Implementations must be
// values — every op returns a NEW view and never mutates the template.
type Invocation interface {
	// RegistryID is the pinned policy-leaf registry identity carried by
	// this view (empty on deferred builds until first dispatch).
	RegistryID() string
	// DispatchContext installs this invocation's role contract into ctx
	// before ANY side effect (the moved withRoleToolContract read).
	DispatchContext(ctx context.Context) context.Context
	// ForIdentity returns the view naming id: RegistryID and the policy
	// registry id set to id; an empty restricted toolset defaults to id.
	ForIdentity(id string) Invocation
	// ForLiveIdentity re-names ONLY the registry identities (the post
	// holdPinned re-pin); the restricted toolset is left untouched.
	ForLiveIdentity(id string) Invocation
}

// constructionOwners acquires and releases ONE Construction owner per
// dispatch on the published registry identity. The ingress mints the owner
// instance id (UUID timing is preserved); implementations translate the
// (registryID, instance) pair onto the root-owned owner key.
type constructionOwners interface {
	// Acquire takes the construction owner under the canonical key; nil
	// return means held.
	Acquire(registryID, instance string) error
	// AcquireConstruction re-creates the construction record and takes its
	// owner (the prune-repair path when Acquire reports a lost record).
	AcquireConstruction(registryID, instance string) error
	// Release drops the owner taken for (registryID, instance).
	Release(registryID, instance string)
}

// DeferredBuild carries the root-owned pin, ingress identity probe and
// construction ownership of a DEFERRED (shared default toolset) build;
// a nil *DeferredBuild on BatchIngressTool marks a PINNED build. The
// functions close over the root binding and the build's leaf registry —
// this type holds no package-agent identifiers itself.
type DeferredBuild struct {
	// Pin pins the build's leaf registry under a fresh identity
	// (batchCoordinatorBinding.pin).
	Pin func() (string, error)
	// RegistryKnown reports whether the ingress still resolves id
	// (BatchWorkIngress.Registry's error only).
	RegistryKnown func(id string) bool
	// Owners is the root capability manager's construction-owner surface.
	Owners constructionOwners
}

// batchIngressTool is the model-facing shell of one batch-armed work tool.
// Run resolves the pinned leaf (hooks/restricted wrappers retained,
// execution wrappers stripped) and enters the ONE authority.
type BatchIngressTool struct {
	fantasy.AgentTool
	Registry *PolicyLeafRegistry // exported: root construction sites arm it
	// Invoke enters the ONE authority for the resolved leaf under the
	// (possibly identity-rewritten) invocation view.
	Invoke func(ctx context.Context, inv Invocation, leaf fantasy.AgentTool, call fantasy.ToolCall) (fantasy.ToolResponse, error)
	// inv is the root invocation template view (typed port, never mutated).
	Inv Invocation
	// bind non-nil marks a DEFERRED build (shared default toolset): the
	// registry identity is minted and its Construction owner taken on the
	// FIRST dispatch, exactly once; every dispatch then holds its own
	// ephemeral Construction owner for the Invoke's duration, so the record
	// stays alive from before invokeFresh's Root owner (pre-Submit) until
	// the executor's Execution owner holds it, and dies by prune after the
	// root's Done + physical dispose (no process-lifetime fallback).
	Bind *DeferredBuild
	// pctChildScope is the exact captured task-agent build scope (nil
	// unless the pinned build captured one).
	PctChildScope *PctChildToolScope
	// captured resolves the leaf's captured delegation registrar (root
	// wrapper traversal); nil result means no captured tool.
	Captured func(leaf fantasy.AgentTool) CapturedRegistrar
	// diskUnavailable reads the root disk-provider sentinel at the use
	// point (error identity preserved; never snapshot eagerly).
	DiskUnavailable func() error
	once            sync.Once
	mu              sync.Mutex
	id              string
	perr            error
}

// ensurePinned publishes the deferred build's registry identity exactly
// once across concurrent dispatchers; the second dispatcher sees the
// published registry. NO Construction owner is taken here — ownership is
// per-dispatch (holdConstruction), so an unused-after-publish toolset
// still returns the manager to zero.
func (t *BatchIngressTool) ensurePinned() (string, error) {
	if !RoleContractsAllowName(RoleContractsForTools([]fantasy.AgentTool{t.AgentTool}), t.Info().Name) {
		return "", ErrRoleToolDenied
	}
	t.once.Do(func() {
		id, err := t.Bind.Pin()
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
func (t *BatchIngressTool) holdConstruction(id string) (func(), error) {
	instance := uuid.NewString()
	if err := t.Bind.Owners.Acquire(id, instance); err == nil {
		return func() { t.Bind.Owners.Release(id, instance) }, nil
	}
	if err := t.Bind.Owners.AcquireConstruction(id, instance); err != nil {
		return nil, err
	}
	return func() { t.Bind.Owners.Release(id, instance) }, nil
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
func (t *BatchIngressTool) holdPinned(id string) (string, func(), error) {
	t.mu.Lock()
	if t.id != "" {
		id = t.id
	}
	t.mu.Unlock()
	release, err := t.holdConstruction(id)
	if err != nil {
		return "", nil, err
	}
	if t.Bind.RegistryKnown(id) {
		return id, release, nil
	}
	release()
	t.mu.Lock()
	fresh, perr := t.Bind.Pin()
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

func (t *BatchIngressTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	// Install the payload's captured ceiling before ANY side effect (including
	// pinning/attaching a batch root). The invocation-time cap and payload cap
	// are both enforced; neither can replace the other.
	ctx = t.Inv.DispatchContext(ctx)
	if err := GuardRoleToolCall(ctx, t.Info().Name); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if call.Name != "" && call.Name != t.Info().Name {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("%v: forged tool name %q does not match %q", ErrRoleToolDenied, call.Name, t.Info().Name)), nil
	}
	// pct: clear inherited delegation state before ordinary work invocations.
	ctx = WithDelegationHandoff(ctx, nil)
	if t.Bind == nil {
		leaf, err := t.Registry.Resolve(call.Name)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		if t.PctChildScope != nil {
			lease := t.PctChildScope.Retain()
			if lease == nil {
				return fantasy.ToolResponse{}, t.DiskUnavailable()
			}
			handoff := newDelegationHandoff([]func(){lease})
			handoff.pctChild = t.PctChildScope
			captured := t.Captured(leaf)
			if captured == nil || !captured.RegisterCapturedHandoff(invocationKey(tools.GetSessionFromContext(ctx), call.ID), handoff) {
				handoff.Release()
				return fantasy.ToolResponse{}, t.DiskUnavailable()
			}
			defer handoff.ReleasePending()
			ctx = WithDelegationHandoff(ctx, handoff)
			return t.Invoke(ctx, t.Inv, leaf, call)
		}
		return t.Invoke(ctx, t.Inv, leaf, call)
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
	leaf, err := t.Registry.Resolve(call.Name)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	inv := t.Inv.ForIdentity(id)
	id, release, err := t.holdPinned(id)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	defer release()
	// holdPinned may have re-pinned after a prune: the invocation must
	// name the LIVE identity, never the stale one.
	inv = inv.ForLiveIdentity(id)
	return t.Invoke(ctx, inv, leaf, call)
}

// privateFetchIngressTool is the private fetch ingress shell: one pinned
// leaf reached only through the pre-built pinnedInvoke.
// CapturedRegistrar is the narrow captured-tool capability the ingress
// needs: registering the handoff for one invocation key under the captured
// tool's own map mutex (the original pctRegister critical section: the
// captured map lock is held across the handoff binding, handoff mutex
// inside captured mutex, and the map insert AND backpointer assignment are
// one atomic registration — never a bare BindCaptured).
type CapturedRegistrar interface {
	RegisterCapturedHandoff(key string, h *PctDelegationHandoff) bool
}

// PrivateFetchIngressTool is the private fetch ingress shell: one pinned
// leaf reached only through the pre-built pinnedInvoke.
type PrivateFetchIngressTool struct {
	fantasy.AgentTool
	// Registry is the pinned leaf registry (exported: the root private
	// fetch build arms it cross-package).
	Registry *PolicyLeafRegistry
	// Invoke is the pre-built pinnedInvoke seam into the ONE authority.
	Invoke func(context.Context, fantasy.AgentTool, fantasy.ToolCall) (fantasy.ToolResponse, error)
}

// restrictedRunWrapped marks the private fetch ingress as an armed policy
// layer (the in-package unexported marker shared with restrictedTool).
func (t *PrivateFetchIngressTool) restrictedRunWrapped() {}

func (t *PrivateFetchIngressTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if err := GuardRoleToolCall(ctx, t.Info().Name); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if call.Name != "" && call.Name != t.Info().Name {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("%v: forged tool name %q does not match %q", ErrRoleToolDenied, call.Name, t.Info().Name)), nil
	}
	leaf, err := t.Registry.ResolveForContext(ctx, t.Info().Name)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	call.Name = t.Info().Name
	return t.Invoke(ctx, leaf, call)
}
