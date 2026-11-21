package agent

import (
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/diskport"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// DiskCapabilityAnchor allows diskport to retain the manager without using it.
func (*diskCapabilityManager) DiskCapabilityAnchor() {}

func diskBindingOwners(caps *DiskProviderRegistry, kind diskOwnerKind) diskport.BindingOwners {
	return diskport.BindingOwners{
		Caps: caps,
		Acquire: func(instance, bindingID string) error {
			return caps.acquire(diskOwnerKey{kind: kind, instance: instance, bindingID: bindingID})
		},
		Release: func(instance, bindingID string) {
			caps.release(diskOwnerKey{kind: kind, instance: instance, bindingID: bindingID})
		},
	}
}

func boundCallOptions(provider tools.DiskProvider, bindingID string, caps *DiskProviderRegistry) *CallOptions {
	return diskport.BoundCallOptions(provider, bindingID, caps)
}

// BindCallDiskProvider initializes trusted call options and returns the Call owner's release token.
func BindCallDiskProvider(caps *DiskProviderRegistry, provider tools.DiskProvider, instance string, opts *CallOptions) (*CallOptions, func(), error) {
	var register func(tools.DiskProvider) (string, error)
	if caps != nil {
		register = caps.RegisterProvider
	}
	return diskport.BindCallDiskProvider(register, diskBindingOwners(caps, diskOwnerCall), provider, instance, opts, ErrNilDiskProviderRegistry)
}

func bindExistingCallDiskBinding(caps *DiskProviderRegistry, bindingID, instance string, opts *CallOptions) (*CallOptions, func(), error) {
	return diskport.BindExistingCallDiskBinding(caps.Lookup, diskBindingOwners(caps, diskOwnerCall), bindingID, instance, opts, ErrDiskProviderUnavailable)
}
