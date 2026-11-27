package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/diskport"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchlife"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	session "github.com/PHPCraftdream/rush/internal/session/service"
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

// releaseResidualContinuations drops every Continuation owner key (graceful
// shutdown: durable rows are picked up by the next process against ITS OWN
// store, so an in-process Continuation owner must not pin the binding
// forever). Call/Construction/Root/Execution owner KEYS are never touched:
// pruneLocked deletes a binding only when its owner set is empty, so a
// binding still retained by live call or execution work survives.
func (m *diskCapabilityManager) releaseResidualContinuations() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.owners {
		if key.kind == diskOwnerContinuation {
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
	if opts != nil && batchlife.ExecutionProviderIntentFrom(ctx, sessionID) == batchlife.ExecutionProviderOS && opts.DiskBindingID == "" && tools.IsOSDisk(opts.DiskProvider) {
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

var (
	ErrDiskProviderNotDurable = diskport.ErrDiskProviderNotDurable

	ErrCredentialSetNotDurable = diskport.ErrCredentialSetNotDurable
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
