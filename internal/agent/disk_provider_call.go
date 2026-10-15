package agent

import (
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// EnsureCallDiskBinding registers once for one immutable CallOptions
// incarnation. Retries reuse its ID; independently built CallOptions each
// mint a new ID without comparing provider interfaces.
func EnsureCallDiskBinding(opts *CallOptions, registry *DiskProviderRegistry) (string, error) {
	if opts == nil {
		return "", nil
	}
	if !tools.IsCustomDiskProvider(opts.DiskProvider) {
		return "", nil
	}
	if registry == nil {
		return "", ErrNilDiskProviderRegistry
	}
	opts.diskBindingMu.Lock()
	defer opts.diskBindingMu.Unlock()
	if opts.diskBindingID != "" {
		return opts.diskBindingID, nil
	}
	if opts.DiskBindingID != "" {
		if opts.diskBindingRegistry == nil {
			return "", ErrDiskProviderUnavailable
		}
		if _, err := opts.diskBindingRegistry.Lookup(opts.DiskBindingID); err != nil {
			return "", err
		}
		return opts.DiskBindingID, nil
	}
	id, err := registry.RegisterProvider(opts.DiskProvider)
	if err != nil {
		return "", err
	}
	opts.diskBindingRegistry = registry
	opts.diskBindingID = id
	opts.DiskBindingID = id
	return id, nil
}

// ReleaseCallDiskBinding releases the call-owner reference. Executor leases
// retain it independently until underlying execution disposal.
func ReleaseCallDiskBinding(opts *CallOptions) {
	if opts == nil {
		return
	}
	opts.diskBindingMu.Lock()
	registry, id := opts.diskBindingRegistry, opts.diskBindingID
	opts.diskBindingRegistry, opts.diskBindingID, opts.DiskBindingID = nil, "", ""
	opts.diskBindingMu.Unlock()
	if registry != nil && id != "" {
		registry.ReleaseProvider(id)
	}
}
