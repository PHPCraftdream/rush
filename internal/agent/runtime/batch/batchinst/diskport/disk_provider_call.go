package diskport

import (
	"fmt"
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// callDiskBinding is the IMMUTABLE binding handle of one trusted call
// state. It is minted exactly once — when the SDK invocation's trusted
// call state is created, BEFORE any provider-backed tool is constructed —
// and then never mutated: copies of the owning CallOptions borrow the same
// pointer, and only an explicit Call-owner token (the release closure
// returned at mint time) releases the owner. There is no lazy
// registration, no re-mint, and no clearing through a copied options
// object.
type callDiskBinding struct {
	id       string
	provider tools.DiskProvider
	caps     CapabilityAnchor
}

// BindingID implements agentcall.CallBindingHandle: the opaque binding
// identity the immutable handle was minted with.
func (b *callDiskBinding) BindingID() string { return b.id }

// newCallDiskBinding mints the binding handle and acquires the Call owner
// keyed by instance. The returned closure releases EXACTLY that owner key
// (idempotent, never descendants). A custom provider and a live App
// manager are required — fail-closed otherwise.
func newCallDiskBinding(register func(tools.DiskProvider) (string, error), owners BindingOwners, provider tools.DiskProvider, instance string, invalidRegistry error) (*callDiskBinding, func(), error) {
	if !tools.IsCustomDiskProvider(provider) || register == nil {
		return nil, nil, invalidRegistry
	}
	bindingID, err := register(provider)
	if err != nil {
		return nil, nil, err
	}
	release, err := owners.retain(instance, bindingID)
	if err != nil {
		return nil, nil, err
	}
	return &callDiskBinding{id: bindingID, provider: provider, caps: owners.Caps}, release, nil
}

// BoundCallOptions builds a CallOptions borrowing an EXISTING immutable
// binding handle (executor-side resolution of a payload's binding ID). No
// owner is acquired here: the execution owner taken by batchExecutor.Start
// retains the binding for the execution's lifetime. The batch agent driver
// attaches its own build-resource scope to the returned options (R12-H2a
// round 2) so the incarnation owns its construction pins.
func BoundCallOptions(provider tools.DiskProvider, bindingID string, caps CapabilityAnchor) *agentcall.CallOptions {
	return &agentcall.CallOptions{
		DiskProvider:  provider,
		DiskBindingID: bindingID,
		DiskBinding:   &callDiskBinding{id: bindingID, provider: provider, caps: caps},
	}
}

// DM5a execution row 2821 owner exception: agentcall.BorrowCallDiskBinding
// already implements the immutable handle read. Diskport does not re-export it.

// BindCallDiskProvider is the trusted-call-state entry point (R12-H2
// §1.2): mints the binding handle, acquires the Call owner keyed by
// instance and initializes the call's build-resource scope ONTO the
// caller's already-populated options (so the rest of the per-call policy
// survives). Returns the same options plus the explicit owner token that
// releases it. The options must not be shared before this returns; copies
// afterwards borrow the handle and the scope.
func BindCallDiskProvider(register func(tools.DiskProvider) (string, error), owners BindingOwners, provider tools.DiskProvider, instance string, opts *agentcall.CallOptions, invalidRegistry error) (*agentcall.CallOptions, func(), error) {
	binding, release, err := newCallDiskBinding(register, owners, provider, instance, invalidRegistry)
	if err != nil {
		return nil, nil, err
	}
	if opts == nil {
		opts = &agentcall.CallOptions{}
	}
	opts.DiskProvider = binding.provider
	opts.DiskBindingID = binding.id
	opts.DiskBinding = binding
	if opts.BuildScope == nil {
		opts.BuildScope = agentcall.NewBuildResourceScope()
	}
	return opts, release, nil
}

// bindExistingCallDiskBinding binds a trusted call state to an EXISTING
// binding (R12-H2b continuation dispatch): same contract as
// BindCallDiskProvider, but no provider is registered — the binding ID
// must already be live in caps, otherwise the typed acquire error fails
// closed. The Call owner keyed by instance+bindingID is acquired here;
// the returned closure releases EXACTLY that owner (idempotent).
func BindExistingCallDiskBinding(lookup func(string) (tools.DiskProvider, error), owners BindingOwners, bindingID, instance string, opts *agentcall.CallOptions, unavailable error) (*agentcall.CallOptions, func(), error) {
	if bindingID == "" {
		return nil, nil, fmt.Errorf("%w: empty binding id", unavailable)
	}
	provider, err := lookup(bindingID)
	if err != nil {
		return nil, nil, err
	}
	release, err := owners.retain(instance, bindingID)
	if err != nil {
		return nil, nil, err
	}
	if opts == nil {
		opts = &agentcall.CallOptions{}
	}
	opts.DiskProvider = provider
	opts.DiskBindingID = bindingID
	opts.DiskBinding = &callDiskBinding{id: bindingID, provider: provider, caps: owners.Caps}
	agentcall.EnsureCallBuildScope(opts)
	return opts, release, nil
}

// CapabilityAnchor keeps the original manager reachable from the binding handle.
// Its marker is never called; owner acquisition and release use separate ports.
type CapabilityAnchor interface {
	DiskCapabilityAnchor()
}

// BindingOwners adapts one owner kind on the app's capability manager.
// Both callbacks address the same (instance, bindingID) key.
type BindingOwners struct {
	Caps    CapabilityAnchor
	Acquire func(instance, bindingID string) error
	Release func(instance, bindingID string)
}

func (o BindingOwners) retain(instance, bindingID string) (func(), error) {
	if err := o.Acquire(instance, bindingID); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { o.Release(instance, bindingID) }) }, nil
}
