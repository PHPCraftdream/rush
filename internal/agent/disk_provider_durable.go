package agent

import (
	"errors"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/diskport"
)

var (
	ErrDiskProviderNotDurable       = diskport.ErrDiskProviderNotDurable
	ErrCredentialBindingUnavailable = diskport.ErrCredentialBindingUnavailable
	ErrCredentialSetNotDurable      = diskport.ErrCredentialSetNotDurable
)

// ErrDiskProviderUnavailable is returned when batch work requires a
// process-bound disk binding that is no longer registered (released,
// drained, or lost to a restart). It never falls back to OS disk.
var ErrDiskProviderUnavailable = errors.New(
	"agent: the process-bound disk provider for this work is no longer available")

func callCarriesDiskProvider(call SessionAgentCall) bool {
	return diskport.CallCarriesDiskProvider(call)
}

func callCarriesNonDurableDependency(call SessionAgentCall) bool {
	return diskport.CallCarriesNonDurableDependency(call)
}

func callCarriesBatchQuestionCapability(call SessionAgentCall) bool {
	return diskport.CallCarriesBatchQuestionCapability(call)
}
