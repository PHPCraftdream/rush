package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"charm.land/catwalk/pkg/catwalk"
)

// RuntimeOverrides holds per-session settings that are never persisted to
// disk. They are applied on top of the loaded Config and survive only for
// the lifetime of the process (or workspace).
type RuntimeOverrides struct {
	SkipPermissionRequests bool
}

// storeSnapshot is an immutable point-in-time view of everything that
// ReloadFromDisk (or a copy-on-write mutator) replaces as one unit. A
// reader that loads a *storeSnapshot via ConfigStore.snap always sees a
// single, internally-consistent generation: config/resolver/knownProviders/
// etc. all came from the same load or the same mutation, never a mix of an
// old and a new generation torn across separate fields.
//
// Fields NOT here (workingDir, globalDataPath) are set once when the
// ConfigStore is constructed and never change afterwards, so they don't
// need to be part of the versioned snapshot.
type storeSnapshot struct {
	config             *Config
	resolver           VariableResolver
	knownProviders     []catwalk.Provider
	loadedPaths        []string // config files that were successfully loaded
	trackedConfigPaths []string // unique, normalized config file paths
	snapshots          map[string]fileSnapshot
	workspacePath      string // .rush/rush.json (recomputed on every reload)
	overrides          RuntimeOverrides

	// generation is a monotonically increasing counter assigned at publish
	// time (see ConfigStore.publishLocked). It exists so a long-running
	// candidate build in reloadFromDiskUnlocked (which runs WITHOUT
	// publishMu — see reloadMu) can detect, at publish time, whether some
	// other writer already published a newer generation while the build
	// was in flight, and rebase instead of silently clobbering it with a
	// candidate built from a now-stale base. Never read by anything
	// outside this CAS check; it is not part of the store's public API.
	generation uint64
}

// clone returns a shallow copy of the snapshot. Callers that need to
// change one field (e.g. workspacePath, overrides) can clone then
// mutate the copy before publishing it — the original snapshot, still
// visible to any reader that loaded it earlier, is never touched.
func (sn *storeSnapshot) clone() *storeSnapshot {
	if sn == nil {
		return &storeSnapshot{}
	}
	c := *sn
	return &c
}

// ConfigStore is the single entry point for all config access. It owns the
// pure-data Config, runtime state (working directory, resolver, known
// providers), and persistence to both global and workspace config files.
//
// Thread-safety model: all state that changes together as one logical
// generation (config, resolver, knownProviders, loadedPaths, overrides,
// workspacePath, staleness tracking) lives in an immutable *storeSnapshot
// published through the snap atomic.Pointer. Readers call snap.Load() once
// and read every field from the same snapshot — lock-free, and always a
// single consistent generation. Writers (both ReloadFromDisk and the
// copy-on-write setters like SetCompactMode) serialize against each other
// via publishMu, build a brand new snapshot from a shallow copy of
// the config plus freshly cloned nested collections, and publish it with a
// single snap.Store — the old snapshot, and anything still holding a
// reference to it, is left completely untouched.
type ConfigStore struct {
	snap atomic.Pointer[storeSnapshot]

	// workingDir and globalDataPath are set once at construction time
	// (Load / NewTestStore) and never mutated afterwards, so they are
	// safe to read without synchronization.
	workingDir     string
	globalDataPath string // ~/.local/share/rush/rush.json

	// publishMu is the single mutex that serialises ALL snapshot
	// publications — both ReloadFromDisk (which rebuilds the entire
	// config from disk) and the copy-on-write mutators (updateConfig,
	// RefreshStalenessSnapshot, CaptureStalenessSnapshot). Every
	// read-current-generation → build-new → snap.Store cycle takes this
	// lock for its full duration, so a writer can never publish a clone
	// built from a stale generation on top of a reload that already
	// published a newer one (the lost-update race that existed when
	// reloadMu and writeMu were separate locks).
	//
	// autoReload's redundant-reload dedup is reloadMu.TryLock() (see
	// reloadMu below), NOT publishMu — Load holds ONLY publishMu for its
	// whole body, never reloadMu, so a re-entrant autoReload call from
	// inside configureProviders (running under Load or a reload) would
	// find reloadMu free, succeed its TryLock, and proceed into
	// buildAndPublishReload, which acquires publishMu itself — on the
	// SAME goroutine already holding it. sync.Mutex is not reentrant in
	// Go, so that self-deadlocks and hangs forever. This is exactly why
	// configureProviders must only ever call removeConfigFieldBestEffort
	// (which never calls autoReload at all) and must NEVER call the full
	// RemoveConfigField/SetConfigFields (which do call autoReload) from a
	// path that runs under a held publishMu.
	publishMu sync.Mutex

	// diskWriteMu serialises the on-disk read-modify-write cycle
	// (os.ReadFile → sjson.Set/Delete → atomicWriteFile) in
	// SetConfigFields and RemoveConfigField. Without it, two concurrent
	// callers writing to the same rush.json path could each read the
	// pre-write file, apply only their own key, and have the second
	// atomicWriteFile clobber the first — a classic lost-update on the
	// file itself, independent of the in-memory snapshot race that
	// publishMu fixes. It is deliberately separate from publishMu so
	// that disk I/O does not block lock-free snapshot readers, and so
	// that the autoReload call at the end of each disk write (which
	// acquires publishMu via ReloadFromDisk) cannot deadlock against a
	// publishMu already held by the same goroutine. Lock ordering is
	// always publishMu → diskWriteMu (when both are held), and
	// diskWriteMu is always released before autoReload is called.
	diskWriteMu sync.Mutex

	// reloadMu serialises the CANDIDATE-BUILD phase of a disk reload
	// (loadFromConfigPaths, workspace merge, configureProviders — which
	// includes every shell-substitution ResolveValue call for provider
	// keys/headers — and configureSelectedModels) against other
	// concurrent reload attempts, WITHOUT holding publishMu. This is the
	// fix for the lock-convoy where a single hung "$(...)" in a config
	// value used to block publishMu — and therefore every reader,
	// including unrelated runtime mutators like SetSkipPermissionRequests
	// or SetProviderRuntimeConfig — for as long as the shell substitution
	// ran (up to resolveTimeout).
	//
	// reloadFromDiskUnlocked takes reloadMu (via defer, held for its
	// whole call), builds a full candidate snapshot from local variables
	// ONLY (no store mutation), then — still holding reloadMu — takes
	// publishMu just long enough to CAS-check the generation and
	// publish. autoReload uses TryLock on reloadMu (not publishMu) to
	// preserve the original "skip a redundant reload when one is already
	// in progress" behaviour without reintroducing the publishMu hold
	// during the expensive candidate-build phase.
	//
	// Lock ordering when both are held: reloadMu is acquired first and
	// held THROUGH the publishMu acquisition (nested, not sequential —
	// reloadMu is NOT released before publishMu is taken). The global
	// order is always reloadMu → publishMu → diskWriteMu → the
	// inter-process config file lock, consistently in every call path
	// that touches more than one of these; there is no path that
	// acquires them in the reverse order. It is this consistent nesting
	// order — not any claim that the two locks are never held together —
	// that rules out a deadlock here.
	reloadMu sync.Mutex
}

// loadSnapshot returns the current published snapshot. It never returns
// nil: the store is always constructed with an initial snapshot.
func (s *ConfigStore) loadSnapshot() *storeSnapshot {
	sn := s.snap.Load()
	if sn == nil {
		// Defensive: should not happen for a store built via Load or
		// NewTestStore, but avoids a nil-pointer panic for any
		// zero-value ConfigStore{} a test might construct directly.
		return &storeSnapshot{}
	}
	return sn
}

// publishLocked assigns the next generation number to next and publishes
// it. The caller MUST already hold publishMu — generation is only ever
// assigned/read while holding that lock, so incrementing it here is not
// itself a race even though the field is also readable lock-free via
// loadSnapshot() (readers only ever compare it back against a value they
// captured under publishMu; see reloadFromDiskUnlocked's CAS check).
func (s *ConfigStore) publishLocked(next *storeSnapshot) {
	next.generation = s.loadSnapshot().generation + 1
	s.snap.Store(next)
}

// Config returns the pure-data config struct (read-only after load).
func (s *ConfigStore) Config() *Config {
	return s.loadSnapshot().config
}

// Generation returns the current generation number of the config snapshot.
// The generation is incremented on every config mutation (reload, set operations, etc.).
// Callers can use this to detect when config has changed and invalidate caches.
func (s *ConfigStore) Generation() uint64 {
	return s.loadSnapshot().generation
}

// Snapshot returns both the config and its generation atomically from a single
// storeSnapshot. This prevents reading config and generation separately and
// getting inconsistent results if a reload occurs between the two reads
// (task #341, P1-3).
func (s *ConfigStore) Snapshot() (*Config, uint64) {
	sn := s.loadSnapshot()
	return sn.config, sn.generation
}

// WorkingDir returns the current working directory.
func (s *ConfigStore) WorkingDir() string {
	return s.workingDir
}

// Resolver returns the variable resolver.
func (s *ConfigStore) Resolver() VariableResolver {
	return s.loadSnapshot().resolver
}

// Resolve resolves a variable reference using the configured resolver.
func (s *ConfigStore) Resolve(key string) (string, error) {
	resolver := s.loadSnapshot().resolver
	if resolver == nil {
		return "", fmt.Errorf("no variable resolver configured")
	}
	return resolver.ResolveValue(key)
}

// KnownProviders returns the list of known providers. The returned slice
// is not copied: reload/mutation always replaces the whole slice (never
// mutates an existing one in place), so a snapshot's backing array is
// never written to after publication and it's safe to hand it out
// directly.
func (s *ConfigStore) KnownProviders() []catwalk.Provider {
	return s.loadSnapshot().knownProviders
}

// SetupAgents (re)configures the coder and task agents on the CURRENT
// generation, through the copy-on-write updateConfig path — it does NOT
// mutate the currently-published *Config in place (that was the P2.1 bug
// this method used to have before being briefly removed as dead code; it
// turned out to still be a real, load-bearing test helper — see
// coordinator_test.go's newRoleModelTestCoordinator/newWorkerToolTestCoordinator/
// TestBuildTools_CoderHasAskQuestion, which call this on a from-scratch
// config where IsConfigured() is false, so config.Load's own
// configureSelectedModels/SetupAgents call never ran and Agents would
// otherwise stay empty). Config.SetupAgents itself only reads
// Options/DisabledTools and assigns a fresh Agents map, so running it on a
// clone and publishing that clone is a correct, race-free way to expose the
// same behavior at the ConfigStore level.
func (s *ConfigStore) SetupAgents() {
	s.updateConfig(func(cfgCopy *Config) {
		cfgCopy.SetupAgents()
	})
}

// Overrides returns the runtime overrides as a value copy. Callers cannot
// mutate the store's internal snapshot state through the returned value —
// use SetSkipPermissionRequests for writes, which goes through the
// copy-on-write publish path (publishMu + clone + atomic Store) so the
// change is visible as a new generation and survives subsequent reloads
// (reloadFromDiskLocked carries overrides forward from prev.overrides).
func (s *ConfigStore) Overrides() RuntimeOverrides {
	return s.loadSnapshot().overrides
}

// SetSkipPermissionRequests sets the SkipPermissionRequests runtime override
// through the copy-on-write path (publishMu + snapshot clone + atomic Store),
// so the change is visible as a new generation and survives subsequent
// reloads (reloadFromDiskLocked carries overrides forward from prev.overrides).
// Unlike the old Overrides() pointer-return pattern, this cannot lose the
// write to a concurrent reload that publishes a new snapshot between the
// caller's read and write.
func (s *ConfigStore) SetSkipPermissionRequests(v bool) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	cur := s.loadSnapshot()
	next := cur.clone()
	next.overrides.SkipPermissionRequests = v
	s.publishLocked(next)
}

// LoadedPaths returns the config file paths that were successfully loaded.
// Returns a copy so callers can't mutate the snapshot's backing array.
func (s *ConfigStore) LoadedPaths() []string {
	return slices.Clone(s.loadSnapshot().loadedPaths)
}

// updateConfig applies a targeted, copy-on-write mutation to the store's
// config and publishes it as a new generation.
//
// It takes publishMu (serialising all copy-on-write mutators AND reloads
// against each other — without it, two concurrent callers could both read
// the same starting snapshot, apply their own change to independent copies,
// and have the second Store() silently discard the first writer's change;
// worse, a concurrent reload's fresh-from-disk generation could be
// silently overwritten by a writer publishing a clone of the pre-reload
// generation),
// shallow-copies the top-level *Config (cfgCopy := *cur.config), and hands
// the mutate callback a pointer to that copy. mutate is responsible for
// cloning any nested map/pointer it intends to change (e.g. Options, a
// map[K]V field) before writing through it — updateConfig only guarantees
// the top-level struct is a fresh copy, not anything it points to. Once
// mutate returns, the new *Config is published as part of a new
// storeSnapshot (resolver/knownProviders/etc. carried over unchanged from
// the snapshot the mutation started from) via a single atomic Store.
//
// This is intentionally synchronous and always publishes — callers that
// also persist to disk (the common case: every setter below writes through
// SetConfigField/SetConfigFields right after) will shortly see their
// change superseded by autoReload's own fresh-from-disk snapshot; that's
// fine and matches pre-refactor semantics, where the in-memory mutation
// was always a best-effort "make the change visible immediately" step
// ahead of the disk-round-trip reload. When autoReload is skipped (no
// workingDir configured, e.g. in unit tests), this in-memory mutation is
// the only durable change, exactly as before.
func (s *ConfigStore) updateConfig(mutate func(cfgCopy *Config)) {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.updateConfigLocked(mutate)
}

// updateConfigLocked applies the same copy-on-write mutation as
// updateConfig but WITHOUT acquiring publishMu. The caller MUST already
// hold publishMu.
//
// It exists for the re-entrant call path inside Load: Load holds
// publishMu, then configureSelectedModels → updatePreferredModelLocked →
// updateConfigLocked applies the in-memory mutation without re-acquiring
// the lock. Without this separation, updateConfig would deadlock on the
// Lock call because the calling goroutine already holds publishMu.
func (s *ConfigStore) updateConfigLocked(mutate func(cfgCopy *Config)) {
	cur := s.loadSnapshot()
	next := cur.clone()

	var cfgCopy Config
	if cur.config != nil {
		cfgCopy = *cur.config
	}
	mutate(&cfgCopy)
	next.config = &cfgCopy

	s.publishLocked(next)
}

// NewTestStore creates a ConfigStore for testing purposes.
func NewTestStore(cfg *Config, loadedPaths ...string) *ConfigStore {
	s := &ConfigStore{}
	s.snap.Store(&storeSnapshot{
		config:      cfg,
		loadedPaths: loadedPaths,
	})
	return s
}

// testStoreOpts configures the fields newTestConfigStore should set on the
// snapshot/store it builds. Only used by this package's own white-box
// tests, which used to build ConfigStore{config: ..., globalDataPath: ...}
// literals directly — that stopped compiling once config/workspacePath
// moved behind the snap atomic.Pointer, so tests now go through this
// helper instead.
type testStoreOpts struct {
	config         *Config
	globalDataPath string
	workspacePath  string
	resolver       VariableResolver
	loadedPaths    []string
}

// newTestConfigStore builds a *ConfigStore for this package's white-box
// tests from the given options, publishing them as a single snapshot the
// same way production code does. globalDataPath is stored directly on the
// ConfigStore (it's the one config-related field that isn't part of the
// snapshot), everything else goes into the initial storeSnapshot.
func newTestConfigStore(opts testStoreOpts) *ConfigStore {
	s := &ConfigStore{globalDataPath: opts.globalDataPath}
	s.snap.Store(&storeSnapshot{
		config:        opts.config,
		workspacePath: opts.workspacePath,
		resolver:      opts.resolver,
		loadedPaths:   opts.loadedPaths,
	})
	return s
}

// LogPath returns the path to the log file.
func (s *ConfigStore) LogPath() string {
	opts := s.loadSnapshot().config.Options
	if opts == nil || opts.DataDirectory == "" {
		return ""
	}
	return filepath.Join(opts.DataDirectory, "logs", "rush.log")
}
