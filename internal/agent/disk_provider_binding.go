package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/google/uuid"
)

// Process-bound disk provider bindings (R12-H2 §3.2/§3.3): an app/client-
// owned IMMUTABLE registry mapping opaque binding IDs onto the caller-
// supplied tools.DiskProvider objects pinned to pending/accepted batch work.
// Binding IDs are minted here and never derived from provider contents;
// providers are never compared (valid provider values need not be
// comparable). Only the opaque ID travels in durable invocation metadata —
// never a pointer, credential or provider state. Lookup fails closed with
// ErrDiskProviderUnavailable; a missing binding never falls back to OS disk.
type diskBindingID string

// diskBindingEntry is one immutable registration. processBound is a
// permanent marker: every entry in this registry is process-local by
// construction and never survives a restart.
type diskBindingEntry struct {
	id           diskBindingID
	provider     tools.DiskProvider
	owner        string
	processBound bool
	refs         int
}

type DiskProviderRegistry = diskProviderRegistry

// diskProviderRegistry is bounded, concurrency-safe, and releasable.
type diskProviderRegistry struct {
	mu      sync.Mutex
	entries map[diskBindingID]*diskBindingEntry
}

// ErrNilDiskProviderRegistry is returned when a required injectable
// registry instance is nil.
var ErrNilDiskProviderRegistry = errors.New("disk provider registry: nil registry")

// NewDiskProviderRegistry builds one isolated registry instance. Hosts
// inject a distinct instance into each App/runtime.
func NewDiskProviderRegistry() *DiskProviderRegistry {
	return &diskProviderRegistry{entries: make(map[diskBindingID]*diskBindingEntry)}
}

// Register pins provider under a fresh opaque binding ID. A nil provider or
// the canonical OS disk is refused (registerDiskBinding maps those to "").
// The owner (app/runtime generation) is recorded when known.
func (r *diskProviderRegistry) Register(provider tools.DiskProvider) (string, error) {
	return r.RegisterForOwner("", provider)
}

// RegisterProvider is the host-facing registration API for one app-owned
// registry. CallOptions retain the returned immutable ID for their lifetime.
func (r *diskProviderRegistry) RegisterProvider(provider tools.DiskProvider) (string, error) {
	return r.Register(provider)
}

// AcquireProvider leases a binding for pending/admitted work and returns the
// provider plus a release closure to invoke only after execution disposal.
func (r *diskProviderRegistry) AcquireProvider(id string) (tools.DiskProvider, func(), error) {
	return r.Acquire(id)
}

// ReleaseProvider drops a binding whose owner and execution leases ended.
func (r *diskProviderRegistry) ReleaseProvider(id string) { r.Release(id) }

// RegisterForOwner is Register with an explicit owning app/runtime marker.
func (r *diskProviderRegistry) RegisterForOwner(owner string, provider tools.DiskProvider) (string, error) {
	if r == nil {
		return "", ErrNilDiskProviderRegistry
	}
	if !tools.HasDiskProvider(provider) || isNilDiskProvider(provider) || tools.IsOSDisk(provider) {
		return "", errors.New("disk provider registry: only a custom provider is registrable")
	}
	id := diskBindingID(uuid.NewString())
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[diskBindingID]*diskBindingEntry)
	}
	r.entries[id] = &diskBindingEntry{id: id, provider: provider, owner: owner, processBound: true, refs: 1}
	return string(id), nil
}

func isNilDiskProvider(provider tools.DiskProvider) bool {
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Lookup resolves id to the registered provider. Unknown/released bindings
// fail closed with ErrDiskProviderUnavailable — never nil, never OS disk.
func (r *diskProviderRegistry) Lookup(id string) (tools.DiskProvider, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: nil registry", ErrDiskProviderUnavailable)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[diskBindingID(id)]
	if !ok || entry == nil || !tools.HasDiskProvider(entry.provider) || isNilDiskProvider(entry.provider) {
		return nil, fmt.Errorf("%w: binding %q", ErrDiskProviderUnavailable, id)
	}
	return entry.provider, nil
}

// Acquire increments a binding lease before admission/execution starts and
// returns the pinned provider plus an idempotent release closure. Lease
// release is independent of completion publication.
func (r *diskProviderRegistry) Acquire(id string) (tools.DiskProvider, func(), error) {
	if r == nil {
		return nil, nil, fmt.Errorf("%w: nil registry", ErrDiskProviderUnavailable)
	}
	r.mu.Lock()
	entry, ok := r.entries[diskBindingID(id)]
	if !ok || entry == nil || !tools.HasDiskProvider(entry.provider) || isNilDiskProvider(entry.provider) {
		r.mu.Unlock()
		return nil, nil, fmt.Errorf("%w: binding %q", ErrDiskProviderUnavailable, id)
	}
	entry.refs++
	r.mu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { r.releaseLease(id) }) }
	return entry.provider, release, nil
}

func (r *diskProviderRegistry) releaseLease(id string) {
	if r == nil || id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.entries[diskBindingID(id)]
	if entry == nil {
		return
	}
	entry.refs--
	if entry.refs <= 0 {
		delete(r.entries, diskBindingID(id))
	}
}

func (r *diskProviderRegistry) Release(id string) {
	if r == nil || id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.entries[diskBindingID(id)]
	if entry == nil {
		return
	}
	entry.refs--
	if entry.refs <= 0 {
		delete(r.entries, diskBindingID(id))
	}
}

// Drain releases every entry (forced-shutdown retention escape). It never
// publishes or mutates batch state.
func (r *diskProviderRegistry) Drain() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, entry := range r.entries {
		if entry.refs > 0 {
			entry.refs--
		}
		if entry.refs <= 0 {
			delete(r.entries, id)
		}
	}
}

type diskBindingRegistryContextKey struct{}

// withDiskProviderRegistry carries one App-owned registry through trusted
// coordinator/tool-control context construction (never caller JSON).
func withDiskProviderRegistry(ctx context.Context, registry *DiskProviderRegistry) context.Context {
	return context.WithValue(ctx, diskBindingRegistryContextKey{}, registry)
}

func batchDiskRegistryFromContext(ctx context.Context) *DiskProviderRegistry {
	registry, _ := ctx.Value(diskBindingRegistryContextKey{}).(*DiskProviderRegistry)
	return registry
}

// registerDiskBinding maps a caller DiskProvider onto a required binding ID:
// canonical OS and nil map to ""; a custom provider requires an injected
// app-owned registry, and absence fails closed rather than using a global.
func registerDiskBinding(provider tools.DiskProvider, reg *diskProviderRegistry) (string, error) {
	if !tools.HasDiskProvider(provider) || isNilDiskProvider(provider) || tools.IsOSDisk(provider) {
		return "", nil
	}
	if reg == nil {
		return "", ErrNilDiskProviderRegistry
	}
	return reg.Register(provider)
}
