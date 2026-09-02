package agent

// Durable-restart refusal for calls carrying a caller-supplied
// tools.DiskProvider (SDK task #859, design doc §7).
//
// A DiskProvider is arbitrary in-process Go code (typically a closure
// over host state: an open DB handle, an HTTP client, an in-memory map)
// with no serializable form at all — unlike permission.FolderScope,
// which T12 persists as an uncompiled spec and recompiles on restart, a
// disk provider has nothing to mirror. If a call carrying one were
// durably enqueued, a restart would rebuild it with CallOptions.DiskProvider
// == nil, and buildTools' diskOrOS normalizes that nil straight to
// OSDisk() — silently replaying the model's writes onto the operator's
// REAL filesystem instead of the host's. This follows T9's "refuse
// outright" precedent (rejectScopedCallOnCLIProvider), not T12's
// "persist and restore" one: dropping the call is the fail-closed
// direction, since the host is still in-process and can retry.

import "errors"

// ErrDiskProviderNotDurable is returned instead of enqueueing (or
// rebuilding) a call that carries a caller-supplied DiskProvider.
var ErrDiskProviderNotDurable = errors.New(
	"agent: a call with a caller-supplied disk provider cannot be durably queued")

// callCarriesDiskProvider reports whether call's per-call options carry a
// host-supplied filesystem that must never reach the durable run queue.
func callCarriesDiskProvider(call SessionAgentCall) bool {
	return call.CallOptions != nil && call.CallOptions.DiskProvider != nil
}
