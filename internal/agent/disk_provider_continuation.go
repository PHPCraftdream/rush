package agent

import (
	"context"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/diskport"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// Keep the root-facing shape for existing drain, wake and queue callsites.
type preparedContinuation struct {
	port        *diskport.PreparedContinuation
	requirement session.SessionExecutionRequirement
	provider    tools.DiskProvider
	release     func()
}

func (c *coordinator) PrepareContinuation(ctx context.Context, sessionID string, source *session.SessionExecutionRequirement) (*preparedContinuation, error) {
	var load func(context.Context, string) (*session.SessionExecutionRequirement, error)
	if c.execReqs != nil {
		load = c.execReqs.LoadSessionExecutionRequirement
	}
	var lookup func(string) (tools.DiskProvider, error)
	caps := c.diskCaps
	if caps != nil {
		lookup = caps.Lookup
	}
	p, err := diskport.PrepareContinuation(ctx, sessionID, source, load, lookup, continuationOwners(caps), ErrDiskProviderUnavailable)
	if p == nil {
		return nil, err
	}
	return &preparedContinuation{port: p, requirement: p.Requirement, provider: p.Provider, release: p.Release}, nil
}

func (p *preparedContinuation) restorePolicyBaseline(ctx context.Context, sessionID string, opts *CallOptions) (*permission.RunAllowlistSpec, error) {
	return p.port.RestorePolicyBaseline(ctx, sessionID, opts, ErrDiskProviderUnavailable)
}

func (p *preparedContinuation) applyToOptions(opts *CallOptions) { p.port.ApplyToOptions(opts) }

func (p *preparedContinuation) applyTo(call *SessionAgentCall) {
	if p != nil {
		p.port.ApplyTo(call)
	}
}

func continuationOwners(caps *DiskProviderRegistry) diskport.BindingOwners {
	if caps == nil {
		return diskport.BindingOwners{}
	}
	return diskBindingOwners(caps, diskOwnerContinuation)
}

func holdDurableContinuationOwner(caps *DiskProviderRegistry, bindingID, rowKey string) (func(), bool) {
	return diskport.HoldDurableContinuationOwner(continuationOwners(caps), bindingID, rowKey)
}

func releaseDurableContinuationOwner(caps *DiskProviderRegistry, bindingID, rowKey string) {
	diskport.ReleaseDurableContinuationOwner(continuationOwners(caps), bindingID, rowKey)
}

// HoldDurableContinuationOwnerForTest pins the binding using the fixture's exact durable row ID.
func HoldDurableContinuationOwnerForTest(caps *DiskProviderRegistry, bindingID, rowKey string) (func(), bool) {
	return diskport.HoldDurableContinuationOwnerForTest(continuationOwners(caps), bindingID, rowKey)
}

// guardTurnDiskFence is the ordinary-turn fence check (R12-H2b §2.4
// "Ordinary/idle rebuilt calls"): run before provider-sensitive tool
// resolution. A session with a persisted fence must not run a turn that
// cannot name a live binding; an explicit SDK run establishing a new
// epoch passes (its own binding supersedes), and an explicit canonical
// OS provider (sdk.OSDiskProvider()) is the same new-epoch operator
// choice with no binding to name — it also passes. Returns nil for
// unfenced sessions.
func (c *coordinator) guardTurnDiskFence(ctx context.Context, sessionID string, opts *CallOptions) error {
	// An EXPLICIT canonical OS provider on the call options is a
	// deliberate operator choice (sdk.OSDiskProvider()), never a silent
	// fallback: it starts a NEW execution epoch for the session and its
	// run supersedes the stored fence at admission (internal/app
	// ExecuteRun). It must pass without consulting a stale fence that may
	// name a binding this process no longer holds.
	if opts != nil && executionProviderIntentFrom(ctx, sessionID) == ExecutionProviderOS && opts.DiskBindingID == "" && tools.IsOSDisk(opts.DiskProvider) {
		return nil
	}
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

func requirementFromCall(call SessionAgentCall) *session.SessionExecutionRequirement {
	return diskport.RequirementFromCall(call)
}
