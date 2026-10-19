package agent

// AGENT-side continuation core (R12-H2b §2.3): resolving a durable
// session-execution fence into a live, owner-retained binding for a
// continuation (wake/drain) call. A resolved continuation either carries
// the fence's binding identity forward, or fails closed with
// ErrDiskProviderUnavailable — it never falls back to OS disk and never
// silently adopts a different provider.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
)

// preparedContinuation is one resolved continuation capability (R12-H2b
// §2.3): the requirement to carry forward, the live provider behind the
// binding, and the Continuation owner token keeping the binding alive
// until the handoff transfers or is refused. release is idempotent.
type preparedContinuation struct {
	requirement session.SessionExecutionRequirement
	provider    tools.DiskProvider
	caps        *diskCapabilityManager
	release     func()
}

// PrepareContinuation resolves the execution requirement for one
// continuation call of sessionID (R12-H2b §2.3 steps 1-5). The source
// envelope, when non-nil, wins over the stored fence: a new epoch never
// redirects an existing continuation (design §2.2). A missing fence and
// source is the ordinary OS-disk path and returns (nil, nil); any load
// error, empty binding ID, missing capability manager or lost binding
// fails closed with ErrDiskProviderUnavailable. Results are deliberately
// not cached: the manager's owner set is the only authoritative liveness
// record, and a cached resolution could outlive the binding it named.
func (c *coordinator) PrepareContinuation(ctx context.Context, sessionID string, source *session.SessionExecutionRequirement) (*preparedContinuation, error) {
	target := source
	if target == nil && c.execReqs != nil {
		fence, err := c.execReqs.LoadSessionExecutionRequirement(ctx, sessionID)
		if err != nil {
			// A DB/read error is never "no fence": degrading would
			// silently run a provider-bound continuation on OS disk.
			return nil, fmt.Errorf("%w: session %s execution fence is unreadable: %s",
				ErrDiskProviderUnavailable, sessionID, err)
		}
		target = fence
	}
	if target == nil {
		return nil, nil
	}
	if target.BindingID == "" {
		// Defensive: the loader rejects this too; fail closed typed.
		return nil, fmt.Errorf("%w: session %s execution requirement has no binding id",
			ErrDiskProviderUnavailable, sessionID)
	}
	if c.diskCaps == nil {
		return nil, fmt.Errorf("%w: session %s: no capability manager is bound",
			ErrDiskProviderUnavailable, sessionID)
	}
	provider, err := c.diskCaps.Lookup(target.BindingID)
	if err != nil {
		return nil, err
	}
	key := diskOwnerKey{kind: diskOwnerContinuation, instance: uuid.NewString(), bindingID: target.BindingID}
	if err := c.diskCaps.acquire(key); err != nil {
		return nil, err
	}
	caps := c.diskCaps
	var once sync.Once
	return &preparedContinuation{
		requirement: *target,
		provider:    provider,
		caps:        caps,
		release: func() {
			once.Do(func() { caps.release(key) })
		},
	}, nil
}

// restorePolicyBaseline restores the persisted policy baseline (R12-H2b
// §2.3, audit gh#11b) onto opts: the fence's call policy (DisableSubAgents,
// model role, timeout-watchdog policy), the compiled folder scope
// canonicalized against the SAME live provider, and the serializable
// restricted-run spec, which is returned so the caller can arm it on the
// turn context. An unsupported call policy refuses the continuation (typed
// ErrDiskProviderUnavailable — terminal for this epoch); a folder scope
// that cannot canonicalize or compile degrades to a compiled
// deny-everything scope, the same fail-closed direction the durable
// rebuild applies (T12).
func (p *preparedContinuation) restorePolicyBaseline(ctx context.Context, sessionID string, opts *CallOptions) (*permission.RunAllowlistSpec, error) {
	if p.requirement.CallPolicy != nil {
		policyOpts, err := fromSessionCallOptionsSpec(p.requirement.CallPolicy)
		if err != nil {
			return nil, fmt.Errorf("%w: session %s execution fence carries an incompatible call policy: %s",
				ErrDiskProviderUnavailable, sessionID, err)
		}
		if policyOpts == nil {
			policyOpts = &CallOptions{}
		}
		// Carry the binding identity/scope over: the policy spec decodes to a
		// bare CallOptions with no provider knowledge of its own.
		policyOpts.DiskProvider = opts.DiskProvider
		policyOpts.DiskBindingID = opts.DiskBindingID
		policyOpts.diskBinding = opts.diskBinding
		policyOpts.Requirement = opts.Requirement
		policyOpts.buildScope = opts.buildScope
		*opts = *policyOpts
	}
	if p.requirement.FolderScopeSpec != nil {
		var compiled permission.FolderScope
		canonSpec, canonErr := tools.CanonicalizeFolderScopeSpec(ctx, p.provider, *fromSessionFolderScopeSpec(p.requirement.FolderScopeSpec))
		if canonErr != nil {
			slog.Error("restorePolicyBaseline: the fence's folder-scope spec failed to canonicalize; scoping the continuation to deny-everything",
				"session_id", sessionID, "err", canonErr)
		} else {
			var scopeErr error
			compiled, scopeErr = permission.BuildFolderScope(canonSpec)
			if scopeErr != nil {
				slog.Error("restorePolicyBaseline: the fence's folder-scope spec failed to compile; scoping the continuation to deny-everything",
					"session_id", sessionID, "err", scopeErr)
			}
		}
		opts.FolderScope = &compiled
	}
	if p.requirement.RunAllowlistSpec != nil {
		return fromSessionRunAllowlistSpec(p.requirement.RunAllowlistSpec), nil
	}
	return nil, nil
}

// applyToOptions stamps the resolved capability onto a CallOptions value
// (R12-H2b §2.2): provider, binding id and requirement envelope land on the
// options the same way the dispatch stamp does, so a pinned toolset built
// from these options is provider-backed. opts must be non-nil.
func (p *preparedContinuation) applyToOptions(opts *CallOptions) {
	EnsureCallBuildScope(opts)
	requirement := p.requirement
	opts.DiskProvider = p.provider
	opts.DiskBindingID = p.requirement.BindingID
	opts.Requirement = &requirement
	opts.diskBinding = &callDiskBinding{id: p.requirement.BindingID, provider: p.provider, caps: p.caps}
}

// applyTo stamps the resolved capability onto the call for dispatch
// (wake/drain paths). The prepared token's Continuation owner IS the
// retention for the dispatch window: the binding handle is borrowed
// WITHOUT acquiring a second owner. nil receiver is a no-op.
func (p *preparedContinuation) applyTo(call *SessionAgentCall) {
	if p == nil || call == nil {
		return
	}
	if call.CallOptions == nil {
		call.CallOptions = &CallOptions{}
	}
	p.applyToOptions(call.CallOptions)
}

// holdDurableContinuationOwner pins the binding for one durable row's
// wait window (R12-H2b §2.4): the owner key's instance IS the row's
// durable id, so the pump rebuild can release exactly this owner in both
// outcomes (transfer-to-call or terminal refusal). ok=false means the
// binding is already lost — the caller must refuse typed and not enqueue.
func holdDurableContinuationOwner(caps *DiskProviderRegistry, bindingID, rowKey string) (release func(), ok bool) {
	if caps == nil || bindingID == "" || rowKey == "" {
		return nil, false
	}
	key := diskOwnerKey{kind: diskOwnerContinuation, instance: rowKey, bindingID: bindingID}
	if err := caps.acquire(key); err != nil {
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(func() { caps.release(key) }) }, true
}

// releaseDurableContinuationOwner releases the row-keyed owner a durable
// enqueue producer held. Idempotent; safe when absent (an empty row key
// releases nothing — a caller that never had an id must not guess one).
func releaseDurableContinuationOwner(caps *DiskProviderRegistry, bindingID, rowKey string) {
	if caps == nil || bindingID == "" || rowKey == "" {
		return
	}
	caps.release(diskOwnerKey{kind: diskOwnerContinuation, instance: rowKey, bindingID: bindingID})
}

// guardTurnDiskFence is the ordinary-turn fence check (R12-H2b §2.4
// "Ordinary/idle rebuilt calls"): run before provider-sensitive tool
// resolution. A session with a persisted fence must not run a turn that
// cannot name a live binding; an explicit SDK run establishing a new
// epoch passes (its own binding supersedes). Returns nil for unfenced
// sessions.
func (c *coordinator) guardTurnDiskFence(ctx context.Context, sessionID string, opts *CallOptions) error {
	var source *session.SessionExecutionRequirement
	if opts != nil {
		source = opts.Requirement
	}
	prep, err := c.PrepareContinuation(ctx, sessionID, source)
	if err != nil {
		return err
	}
	if prep == nil {
		return nil
	}
	defer prep.release()
	if opts == nil || opts.DiskBindingID == "" {
		return fmt.Errorf("%w: session %s requires its process-bound disk provider; supply it with an explicit SDK run (R12-H2b)", ErrDiskProviderUnavailable, sessionID)
	}
	// A binding mismatch is a new epoch (an explicit run re-bound the
	// session); its own binding supersedes the fence, so it passes.
	return nil
}

// requirementFromCall resolves the requirement envelope a call carries
// (thin alias for diskRequirementForCall, so producer call sites read
// clearly).
func requirementFromCall(call SessionAgentCall) *session.SessionExecutionRequirement {
	return diskRequirementForCall(call)
}

// SetCoordinatorExecutionRequirementStore wires the durable
// session-execution-fence store onto a coordinator (internal/app
// composition root), mirroring the SetBatchDriverClaimer setter pattern.
func SetCoordinatorExecutionRequirementStore(c Coordinator, store session.SessionExecutionRequirementStore) error {
	if c == nil {
		return errors.New("execution requirement store: nil coordinator")
	}
	if store == nil {
		return errors.New("execution requirement store: nil store")
	}
	co, ok := c.(*coordinator)
	if !ok {
		return errors.New("execution requirement store: unsupported coordinator type")
	}
	co.execReqs = store
	return nil
}
