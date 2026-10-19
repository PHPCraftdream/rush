package agent

// One App-owned capability manager (R12-H2 §1): the SINGLE authority that
// owns every provider binding, every pinned construction record and every
// owner that retains them. There are no separate provider/registry/root
// reference counters — exactly ONE set of typed owner keys, where a key is
// (kind, unique-instance-id, optional binding id, optional registry id).
// Repeated acquisition of the SAME key is idempotent; independent
// acquisitions require independent instance IDs; Release removes exactly
// that key. Deletion decisions happen under the manager lock: a
// construction record is deleted when no owner names its registry, a
// binding is deleted when its owner set is empty. Any diagnostic count is
// derived from the set, never stored.

import (
	"context"
	"fmt"
	"sync"
)

// diskOwnerKind is the typed kind of one owner key.
type diskOwnerKind string

const (
	// diskOwnerCall retains the provider for one trusted SDK invocation,
	// WITHOUT retaining every previously constructed toolset.
	diskOwnerCall diskOwnerKind = "call"
	// diskOwnerConstruction retains one pinned construction (decorated
	// leaves + policy) while the call/build scope that may still use it is
	// open.
	diskOwnerConstruction diskOwnerKind = "construction"
	// diskOwnerRoot covers accepted-but-not-started children of one root.
	diskOwnerRoot diskOwnerKind = "root"
	// diskOwnerExecution covers one accepted execution until its physical
	// disposal succeeds.
	diskOwnerExecution diskOwnerKind = "execution"
	// diskOwnerContinuation is RESERVED for H2b durable continuations; it
	// is intentionally unused in H2a.
	diskOwnerContinuation diskOwnerKind = "continuation"
)

// diskOwnerKey is one owner. The zero value is never a valid owner.
type diskOwnerKey struct {
	kind      diskOwnerKind
	instance  string
	bindingID string
	// registryID names the construction this owner needs (construction,
	// root and execution owners); empty for call owners.
	registryID string
}

// buildResourceScope is the closeable build-resource scope of one trusted
// call state: every construction pin performed for the call registers its
// construction release here, so closing the scope releases exactly those
// construction owners and nothing else. Copies of the owning CallOptions
// borrow the same scope pointer. A scope is closed once on resolution
// failure, call rejection, queue cancellation or final call completion;
// close is idempotent.
type buildResourceScope struct {
	mu       sync.Mutex
	releases []func()
	closed   bool
}

func newBuildResourceScope() *buildResourceScope { return &buildResourceScope{} }

// register attaches one construction release to the scope. Registering on
// an already-closed scope releases immediately (the scope's lifetime has
// ended; the pin must not outlive it).
func (s *buildResourceScope) register(release func()) {
	if s == nil || release == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		release()
		return
	}
	s.releases = append(s.releases, release)
	s.mu.Unlock()
}

// registeredCount returns how many releases the still-open scope currently
// holds. Test-support oracle: a closed scope's count is the number of pins
// its close (or immediate release on register-after-close) has already run.
func (s *buildResourceScope) registeredCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.releases)
}

// close runs every registered release exactly once.
func (s *buildResourceScope) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	releases := s.releases
	s.releases = nil
	s.mu.Unlock()
	for _, release := range releases {
		release()
	}
}

// diskCapabilityManager is the App-owned owner-set authority. Construct
// with newDiskCapabilityManager (NewDiskProviderRegistry keeps the
// historical constructor name for app wiring).
type diskCapabilityManager struct {
	mu            sync.Mutex
	bindings      map[string]*diskBindingEntry
	constructions map[string]*diskConstructionRecord
	owners        map[diskOwnerKey]struct{}
	// dropRegistry deletes one registry identity from the leaf-registry
	// identity index when its construction record dies. It is NOT an
	// ownership counter — only an identity lookup hook.
	dropRegistry func(registryID string)
}

func (m *diskCapabilityManager) ownerCounts() (owners, constructions int) {
	if m == nil {
		return 0, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.owners), len(m.constructions)
}

// registry: the registry identity plus the binding it was pinned for
// (empty for ordinary OS-disk constructions).
type diskConstructionRecord struct {
	registryID string
	bindingID  string
}

func newDiskCapabilityManager() *diskCapabilityManager {
	return &diskCapabilityManager{
		bindings:      make(map[string]*diskBindingEntry),
		constructions: make(map[string]*diskConstructionRecord),
		owners:        make(map[diskOwnerKey]struct{}),
	}
}

// setRegistryDrop installs the identity-index deletion hook (called once
// during install, before any pin).
func (m *diskCapabilityManager) setRegistryDrop(drop func(registryID string)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropRegistry = drop
}

// acquire is idempotent for the SAME key. A key naming a binding or a
// construction fails closed when that capability no longer exists.
func (m *diskCapabilityManager) acquire(key diskOwnerKey) error {
	if m == nil {
		return fmt.Errorf("%w: nil capability manager", ErrDiskProviderUnavailable)
	}
	if key.instance == "" {
		return fmt.Errorf("%w: owner key needs a unique instance id", ErrDiskProviderUnavailable)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if key.bindingID != "" {
		if _, ok := m.bindings[key.bindingID]; !ok {
			return fmt.Errorf("%w: binding %q", ErrDiskProviderUnavailable, key.bindingID)
		}
	}
	if key.registryID != "" && key.kind != diskOwnerConstruction {
		// Root/execution owners name a construction that must exist.
		if _, ok := m.constructions[key.registryID]; !ok {
			return fmt.Errorf("%w: construction %q", ErrDiskProviderUnavailable, key.registryID)
		}
	}
	if key.kind == diskOwnerConstruction {
		if _, ok := m.constructions[key.registryID]; !ok {
			return fmt.Errorf("%w: construction %q", ErrDiskProviderUnavailable, key.registryID)
		}
	}
	m.owners[key] = struct{}{}
	return nil
}

// acquireConstruction pins the construction record (pinned-once) and takes
// one Construction owner atomically. Re-pinning the same identity is
// refused; a second Construction owner on an existing identity is allowed
// (independent instance IDs) and keeps the record alive until every owner
// releases.
func (m *diskCapabilityManager) acquireConstruction(bindingID, registryID string, instance string) error {
	if m == nil || registryID == "" || instance == "" {
		return fmt.Errorf("%w: construction owner needs a registry and instance id", ErrDiskProviderUnavailable)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.constructions[registryID]; ok {
		return fmt.Errorf("disk capability manager: construction %q is already pinned", registryID)
	}
	if bindingID != "" {
		if _, ok := m.bindings[bindingID]; !ok {
			return fmt.Errorf("%w: binding %q", ErrDiskProviderUnavailable, bindingID)
		}
	}
	m.constructions[registryID] = &diskConstructionRecord{registryID: registryID, bindingID: bindingID}
	m.owners[diskOwnerKey{kind: diskOwnerConstruction, instance: instance, bindingID: bindingID, registryID: registryID}] = struct{}{}
	return nil
}

// release removes exactly the given key and then prunes, under the same
// lock: construction records no owner names are deleted (and dropped from
// the identity index); bindings whose owner set is empty are deleted.
func (m *diskCapabilityManager) release(key diskOwnerKey) {
	if m == nil || key.kind == "" || key.instance == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.owners, key)
	m.pruneLocked()
}

// releaseResidualConstructions drops every Construction owner key (graceful
// shutdown: the calls are gone; an unused toolset must not outlive them).
// Root/execution owner KEYS are never touched. Deleting the Construction
// keys even when root/execution owners still name the same registry is
// CORRECT: pruneLocked deletes a construction RECORD only when NO owner
// names its registry, so a registry still named by live root/execution work
// keeps its record (and its identity-index entry) — only the scope-open
// Construction pins are dropped.
func (m *diskCapabilityManager) releaseResidualConstructions() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.owners {
		if key.kind == diskOwnerConstruction {
			delete(m.owners, key)
		}
	}
	m.pruneLocked()
}

// pruneLocked applies the deletion rules. Callers hold m.mu.
func (m *diskCapabilityManager) pruneLocked() {
	// Delete a construction record when no owner names its registry.
	for id := range m.constructions {
		if m.registryNamedLocked(id) {
			continue
		}
		delete(m.constructions, id)
		if m.dropRegistry != nil {
			m.dropRegistry(id)
		}
	}
	// Delete a binding when its owner set is empty.
	for id, entry := range m.bindings {
		if m.bindingRetainedLocked(id) {
			continue
		}
		delete(m.bindings, id)
		entry.owners = nil
	}
}

func (m *diskCapabilityManager) registryNamedLocked(registryID string) bool {
	for key := range m.owners {
		if key.registryID == registryID {
			return true
		}
	}
	return false
}

func (m *diskCapabilityManager) bindingRetainedLocked(bindingID string) bool {
	for key := range m.owners {
		if key.bindingID == bindingID {
			return true
		}
	}
	return false
}

// ownerCount is a diagnostic DERIVED from the set — never a stored counter.
func (m *diskCapabilityManager) ownerCount() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.owners)
}

// withDiskCapabilityManager carries the App-owned manager through trusted
// coordinator/tool-control context construction (never caller JSON).
func withDiskCapabilityManager(ctx context.Context, manager *diskCapabilityManager) context.Context {
	return context.WithValue(ctx, diskCapabilityManagerContextKey{}, manager)
}

type diskCapabilityManagerContextKey struct{}

func diskCapabilityManagerFrom(ctx context.Context) *diskCapabilityManager {
	manager, _ := ctx.Value(diskCapabilityManagerContextKey{}).(*diskCapabilityManager)
	return manager
}
