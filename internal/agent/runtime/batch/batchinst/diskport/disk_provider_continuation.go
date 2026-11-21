package diskport

// AGENT-side continuation core (R12-H2b §2.3): resolving a durable
// session-execution fence into a live, owner-retained binding for a
// continuation (wake/drain) call. A resolved continuation either carries
// the fence's binding identity forward, or fails closed with
// ErrDiskProviderUnavailable — it never falls back to OS disk and never
// silently adopts a different provider.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// PreparedContinuation is one resolved continuation capability (R12-H2b
// §2.3): the requirement to carry forward, the live provider behind the
// binding, and the Continuation owner token keeping the binding alive
// until the handoff transfers or is refused. release is idempotent.
type PreparedContinuation struct {
	Requirement session.SessionExecutionRequirement
	Provider    tools.DiskProvider
	Release     func()
	caps        CapabilityAnchor
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
func PrepareContinuation(ctx context.Context, sessionID string, source *session.SessionExecutionRequirement, load func(context.Context, string) (*session.SessionExecutionRequirement, error), lookup func(string) (tools.DiskProvider, error), owners BindingOwners, unavailable error) (*PreparedContinuation, error) {
	target := source
	if target == nil && load != nil {
		fence, err := load(ctx, sessionID)
		if err != nil {
			// A DB/read error is never "no fence": degrading would
			// silently run a provider-bound continuation on OS disk.
			return nil, fmt.Errorf("%w: session %s execution fence is unreadable: %s",
				unavailable, sessionID, err)
		}
		target = fence
	}
	if target == nil {
		return nil, nil
	}
	if target.BindingID == "" {
		// Defensive: the loader rejects this too; fail closed typed.
		return nil, fmt.Errorf("%w: session %s execution requirement has no binding id",
			unavailable, sessionID)
	}
	if lookup == nil {
		return nil, fmt.Errorf("%w: session %s: no capability manager is bound",
			unavailable, sessionID)
	}
	provider, err := lookup(target.BindingID)
	if err != nil {
		return nil, err
	}
	release, err := owners.retain(uuid.NewString(), target.BindingID)
	if err != nil {
		return nil, err
	}
	return &PreparedContinuation{Requirement: *target, Provider: provider, Release: release, caps: owners.Caps}, nil
}

// RestorePolicyBaseline restores the persisted policy baseline (R12-H2b
// §2.3, audit gh#11b) onto opts: the fence's call policy (DisableSubAgents,
// model role, timeout-watchdog policy), the compiled folder scope
// canonicalized against the SAME live provider, and the serializable
// restricted-run spec, which is returned so the caller can arm it on the
// turn context. An unsupported call policy refuses the continuation (typed
// ErrDiskProviderUnavailable — terminal for this epoch); a folder scope
// that cannot canonicalize or compile degrades to a compiled
// deny-everything scope, the same fail-closed direction the durable
// rebuild applies (T12).
func (p *PreparedContinuation) RestorePolicyBaseline(ctx context.Context, sessionID string, opts *agentcall.CallOptions, unavailable error) (*permission.RunAllowlistSpec, error) {
	if p.Requirement.CallPolicy != nil {
		policyOpts, err := agentcall.FromSessionCallOptionsSpec(p.Requirement.CallPolicy)
		if err != nil {
			return nil, fmt.Errorf("%w: session %s execution fence carries an incompatible call policy: %s",
				unavailable, sessionID, err)
		}
		if policyOpts == nil {
			policyOpts = &agentcall.CallOptions{}
		}
		// Carry the binding identity/scope over: the policy spec decodes to a
		// bare CallOptions with no provider knowledge of its own.
		policyOpts.DiskProvider = opts.DiskProvider
		policyOpts.DiskBindingID = opts.DiskBindingID
		policyOpts.DiskBinding = opts.DiskBinding
		policyOpts.Requirement = opts.Requirement
		policyOpts.BuildScope = opts.BuildScope
		*opts = *policyOpts
	}
	if p.Requirement.FolderScopeSpec != nil {
		var compiled permission.FolderScope
		canonSpec, canonErr := tools.CanonicalizeFolderScopeSpec(ctx, p.Provider, *agentcall.FromSessionFolderScopeSpec(p.Requirement.FolderScopeSpec))
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
	if p.Requirement.RunAllowlistSpec != nil {
		return agentcall.FromSessionRunAllowlistSpec(p.Requirement.RunAllowlistSpec), nil
	}
	return nil, nil
}

// ApplyToOptions stamps the resolved capability onto a CallOptions value
// (R12-H2b §2.2): provider, binding id and requirement envelope land on the
// options the same way the dispatch stamp does, so a pinned toolset built
// from these options is provider-backed. opts must be non-nil.
func (p *PreparedContinuation) ApplyToOptions(opts *agentcall.CallOptions) {
	agentcall.EnsureCallBuildScope(opts)
	requirement := p.Requirement
	opts.DiskProvider = p.Provider
	opts.DiskBindingID = p.Requirement.BindingID
	opts.Requirement = &requirement
	opts.DiskBinding = &callDiskBinding{id: p.Requirement.BindingID, provider: p.Provider, caps: p.caps}
}

// ApplyTo stamps the resolved capability onto the call for dispatch
// (wake/drain paths). The prepared token's Continuation owner IS the
// retention for the dispatch window: the binding handle is borrowed
// WITHOUT acquiring a second owner. nil receiver is a no-op.
func (p *PreparedContinuation) ApplyTo(call *agentcall.SessionAgentCall) {
	if p == nil || call == nil {
		return
	}
	if call.CallOptions == nil {
		call.CallOptions = &agentcall.CallOptions{}
	}
	p.ApplyToOptions(call.CallOptions)
}

// HoldDurableContinuationOwner pins a binding for the exact durable row ID.
func HoldDurableContinuationOwner(owners BindingOwners, bindingID, rowKey string) (func(), bool) {
	if owners.Acquire == nil || bindingID == "" || rowKey == "" {
		return nil, false
	}
	release, err := owners.retain(rowKey, bindingID)
	return release, err == nil
}

// ReleaseDurableContinuationOwner drops only the exact row-keyed owner.
func ReleaseDurableContinuationOwner(owners BindingOwners, bindingID, rowKey string) {
	if owners.Release == nil || bindingID == "" || rowKey == "" {
		return
	}
	owners.Release(rowKey, bindingID)
}

// RequirementFromCall resolves the requirement envelope a call carries
// (thin alias for diskRequirementForCall, so producer call sites read
// clearly).
func RequirementFromCall(call agentcall.SessionAgentCall) *session.SessionExecutionRequirement {
	return agentcall.DiskRequirementForCall(call)
}
