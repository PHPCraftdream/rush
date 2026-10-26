package agent

import (
	"fmt"
	"sync"

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
	caps     *diskCapabilityManager
}

// newCallDiskBinding mints the binding handle and acquires the Call owner
// keyed by instance. The returned closure releases EXACTLY that owner key
// (idempotent, never descendants). A custom provider and a live App
// manager are required — fail-closed otherwise.
func newCallDiskBinding(caps *diskCapabilityManager, provider tools.DiskProvider, instance string) (*callDiskBinding, func(), error) {
	if !tools.IsCustomDiskProvider(provider) {
		return nil, nil, ErrNilDiskProviderRegistry
	}
	if caps == nil {
		return nil, nil, ErrNilDiskProviderRegistry
	}
	bindingID, err := caps.RegisterProvider(provider)
	if err != nil {
		return nil, nil, err
	}
	if err := caps.acquire(diskOwnerKey{kind: diskOwnerCall, instance: instance, bindingID: bindingID}); err != nil {
		return nil, nil, err
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			caps.release(diskOwnerKey{kind: diskOwnerCall, instance: instance, bindingID: bindingID})
		})
	}
	return &callDiskBinding{id: bindingID, provider: provider, caps: caps}, release, nil
}

// boundCallOptions builds a CallOptions borrowing an EXISTING immutable
// binding handle (executor-side resolution of a payload's binding ID). No
// owner is acquired here: the execution owner taken by batchExecutor.Start
// retains the binding for the execution's lifetime. The batch agent driver
// attaches its own build-resource scope to the returned options (R12-H2a
// round 2) so the incarnation owns its construction pins.
func boundCallOptions(provider tools.DiskProvider, bindingID string, caps *DiskProviderRegistry) *CallOptions {
	return &CallOptions{
		DiskProvider:  provider,
		DiskBindingID: bindingID,
		diskBinding:   &callDiskBinding{id: bindingID, provider: provider, caps: caps},
	}
}

// BorrowCallDiskBinding returns the call's minted binding HANDLE for a copy
// of the owning options to borrow (H2a §1.2): copies of the owning options
// borrow the same immutable handle without acquiring a new owner — the
// caller's Call owner retains it. nil when the options carry none (a call
// with no provider binding).
func BorrowCallDiskBinding(opts *CallOptions) *callDiskBinding {
	if opts == nil {
		return nil
	}
	return opts.diskBinding
}

// BorrowDiskBindingFrom copies the binding HANDLE from src into opts (H2a
// §1.2 borrow semantics): a copy of the owning options borrows the same
// immutable handle without acquiring a new owner — src's Call owner retains
// it. buildScope is deliberately left untouched: the borrowing turn's tool
// builds own their scope via the normal per-call setup.
func (opts *CallOptions) BorrowDiskBindingFrom(src *CallOptions) {
	if opts == nil {
		return
	}
	opts.diskBinding = BorrowCallDiskBinding(src)
}

// BindCallDiskProvider is the trusted-call-state entry point (R12-H2
// §1.2): mints the binding handle, acquires the Call owner keyed by
// instance and initializes the call's build-resource scope ONTO the
// caller's already-populated options (so the rest of the per-call policy
// survives). Returns the same options plus the explicit owner token that
// releases it. The options must not be shared before this returns; copies
// afterwards borrow the handle and the scope.
func BindCallDiskProvider(caps *DiskProviderRegistry, provider tools.DiskProvider, instance string, opts *CallOptions) (*CallOptions, func(), error) {
	binding, release, err := newCallDiskBinding(caps, provider, instance)
	if err != nil {
		return nil, nil, err
	}
	if opts == nil {
		opts = &CallOptions{}
	}
	opts.DiskProvider = provider
	opts.DiskBindingID = binding.id
	opts.diskBinding = binding
	if opts.buildScope == nil {
		opts.buildScope = newBuildResourceScope()
	}
	return opts, release, nil
}

// bindExistingCallDiskBinding binds a trusted call state to an EXISTING
// binding (R12-H2b continuation dispatch): same contract as
// BindCallDiskProvider, but no provider is registered — the binding ID
// must already be live in caps, otherwise the typed acquire error fails
// closed. The Call owner keyed by instance+bindingID is acquired here;
// the returned closure releases EXACTLY that owner (idempotent).
func bindExistingCallDiskBinding(caps *DiskProviderRegistry, bindingID string, instance string, opts *CallOptions) (*CallOptions, func(), error) {
	if bindingID == "" {
		return nil, nil, fmt.Errorf("%w: empty binding id", ErrDiskProviderUnavailable)
	}
	provider, err := caps.Lookup(bindingID)
	if err != nil {
		return nil, nil, err
	}
	if err := caps.acquire(diskOwnerKey{kind: diskOwnerCall, instance: instance, bindingID: bindingID}); err != nil {
		return nil, nil, err
	}
	if opts == nil {
		opts = &CallOptions{}
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			caps.release(diskOwnerKey{kind: diskOwnerCall, instance: instance, bindingID: bindingID})
		})
	}
	opts.DiskProvider = provider
	opts.DiskBindingID = bindingID
	opts.diskBinding = &callDiskBinding{id: bindingID, provider: provider, caps: caps}
	EnsureCallBuildScope(opts)
	return opts, release, nil
}

// EnsureCallBuildScope returns the trusted call state's build-resource
// scope, creating it first when the options carry none yet (ordinary,
// non-provider calls). It must be called before the options are shared;
// copies borrow the same scope.
func EnsureCallBuildScope(opts *CallOptions) *buildResourceScope {
	if opts == nil {
		return nil
	}
	if opts.buildScope == nil {
		opts.buildScope = newBuildResourceScope()
	}
	return opts.buildScope
}
