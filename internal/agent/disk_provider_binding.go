package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/google/uuid"
)

// Process-bound disk provider bindings (R12-H2 §3.2/§3.3): an app/client-
// owned IMMUTABLE mapping of opaque binding IDs onto the caller-supplied
// tools.DiskProvider objects pinned to pending/accepted batch work. The
// mapping is absorbed into the App-owned capability manager
// (disk_provider_capability.go): registration mints a binding with an
// EMPTY owner set; only owner keys retain it, and it is deleted when its
// owner set becomes empty. Binding IDs are minted here and never derived
// from provider contents; providers are never compared (valid provider
// values need not be comparable). Only the opaque ID travels in durable
// invocation metadata — never a pointer, credential or provider state.
// Lookup fails closed with ErrDiskProviderUnavailable; a missing binding
// never falls back to OS disk.
type diskBindingID string

// diskBindingEntry is one immutable registration: opaque ID, borrowed
// provider (Rush NEVER closes it), App/host/Runner generation marker, and
// the owner keys that retain it. There is no reference counter — the
// owners map IS the retention state, and the binding dies when it empties.
type diskBindingEntry struct {
	id           diskBindingID
	provider     tools.DiskProvider
	owner        string
	processBound bool
	owners       map[diskOwnerKey]struct{}
}

// DiskProviderRegistry is the exported name of the App-owned capability
// manager. The historical registry API is deliberately reduced to
// registration and fail-closed lookup: all retention is expressed through
// typed owner keys on the manager, never through refs.
type DiskProviderRegistry = diskCapabilityManager

// NewDiskProviderRegistry builds one isolated capability manager instance.
// Hosts inject a distinct instance into each App/runtime.
func NewDiskProviderRegistry() *DiskProviderRegistry {
	return newDiskCapabilityManager()
}

func (m *diskCapabilityManager) OwnerDiagnostics() (owners, constructions int, registries int) {
	if m == nil {
		return 0, 0, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.owners), len(m.constructions), len(m.constructions)
}

// manager instance is nil.
var ErrNilDiskProviderRegistry = errors.New("disk provider registry: nil registry")

// Register pins provider under a fresh opaque binding ID with an empty
// owner set. A nil provider or the canonical OS disk is refused
// (registerDiskBinding maps those to ""). The owner (app/runtime
// generation) is recorded when known.
func (m *diskCapabilityManager) Register(provider tools.DiskProvider) (string, error) {
	return m.RegisterForOwner("", provider)
}

// RegisterProvider is the host-facing registration API for one app-owned
// manager. CallOptions retain the returned immutable ID for their
// lifetime.
func (m *diskCapabilityManager) RegisterProvider(provider tools.DiskProvider) (string, error) {
	return m.Register(provider)
}

// RegisterForOwner is Register with an explicit owning app/runtime marker.
func (m *diskCapabilityManager) RegisterForOwner(owner string, provider tools.DiskProvider) (string, error) {
	if m == nil {
		return "", ErrNilDiskProviderRegistry
	}
	if !tools.HasDiskProvider(provider) || isNilDiskProvider(provider) || tools.IsOSDisk(provider) {
		return "", errors.New("disk provider registry: only a custom provider is registrable")
	}
	id := diskBindingID(uuid.NewString())
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bindings == nil {
		m.bindings = make(map[string]*diskBindingEntry)
	}
	if m.owners == nil {
		m.owners = make(map[diskOwnerKey]struct{})
	}
	m.bindings[string(id)] = &diskBindingEntry{id: id, provider: provider, owner: owner, processBound: true, owners: make(map[diskOwnerKey]struct{})}
	return string(id), nil
}

// EnsureUsableDiskProvider reports whether a caller-supplied DiskProvider
// override is safe to touch. A nil interface, the canonical OS disk, or a
// genuinely usable custom provider is accepted; a TYPED-NIL custom provider
// (a non-nil interface holding a nil concrete value, e.g. (*myDisk)(nil))
// is rejected: it passes tools.HasDiskProvider but panics on any method call,
// so it must fail fast BEFORE any disk-touching work (folder-scope
// canonicalization, provider traffic).
func EnsureUsableDiskProvider(provider tools.DiskProvider) error {
	if !tools.HasDiskProvider(provider) || tools.IsOSDisk(provider) {
		return nil
	}
	if isNilDiskProvider(provider) {
		return errors.New("invalid DiskProvider override: the provider is a nil-valued custom DiskProvider (typed nil); supply a usable custom provider or leave RunOverrides.DiskProvider unset")
	}
	return nil
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
func (m *diskCapabilityManager) Lookup(id string) (tools.DiskProvider, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: nil registry", ErrDiskProviderUnavailable)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.bindings[id]
	if !ok || entry == nil || !tools.HasDiskProvider(entry.provider) || isNilDiskProvider(entry.provider) {
		return nil, fmt.Errorf("%w: binding %q", ErrDiskProviderUnavailable, id)
	}
	return entry.provider, nil
}

type diskBindingRegistryContextKey struct{}

// withDiskProviderRegistry carries one App-owned manager through trusted
// coordinator/tool-control context construction (never caller JSON).
func withDiskProviderRegistry(ctx context.Context, registry *DiskProviderRegistry) context.Context {
	return withDiskCapabilityManager(ctx, registry)
}

func batchDiskRegistryFromContext(ctx context.Context) *DiskProviderRegistry {
	return diskCapabilityManagerFrom(ctx)
}

// registerDiskBinding maps a caller DiskProvider onto a required binding ID:
// canonical OS and nil map to ""; a custom provider requires an injected
// app-owned manager, and absence fails closed rather than using a global.
func registerDiskBinding(provider tools.DiskProvider, reg *diskCapabilityManager) (string, error) {
	if !tools.HasDiskProvider(provider) || isNilDiskProvider(provider) || tools.IsOSDisk(provider) {
		return "", nil
	}
	if reg == nil {
		return "", ErrNilDiskProviderRegistry
	}
	return reg.Register(provider)
}
