// Package mcpstore owns MCP persistence: lifecycle transactions over the
// rush/global/system config slots, the admission boundary, and the durable
// uncertainty fence. It never imports the root config package; every core
// store primitive it needs arrives through Port.
package mcpstore

import (
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/config/model/envresolve"
	"github.com/PHPCraftdream/rush/internal/config/store/settings"
	"github.com/PHPCraftdream/rush/internal/kit/fs/home"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell"
)

// Model contracts re-exported from cfgmodel (same alias pattern as the root
// facade).
type (
	Config                 = cfgmodel.Config
	MCPConfig              = cfgmodel.MCPConfig
	MCPs                   = cfgmodel.MCPs
	MCPOrigin              = cfgmodel.MCPOrigin
	Scope                  = cfgmodel.Scope
	SettingsWriteAuthority = cfgmodel.SettingsWriteAuthority
	VariableResolver       = envresolve.VariableResolver

	// io aliases (moved with the store files they served).
	fileSnapshot          = docread.FileSnapshot
	stableConfigDocument  = docread.StableConfigDocument
	reloadFileFingerprint = docread.Fingerprint
)

// Constants re-exported from cfgmodel.
const (
	DockerMCPName              = cfgmodel.DockerMCPName
	MCPHttp                    = cfgmodel.MCPHttp
	MCPOriginExternal          = cfgmodel.MCPOriginExternal
	MCPOriginGlobal            = cfgmodel.MCPOriginGlobal
	MCPOriginProject           = cfgmodel.MCPOriginProject
	MCPOriginSystem            = cfgmodel.MCPOriginSystem
	MCPOriginWorkspace         = cfgmodel.MCPOriginWorkspace
	MCPSourceExternal          = cfgmodel.MCPSourceExternal
	MCPStdio                   = cfgmodel.MCPStdio
	ScopeGlobal                = cfgmodel.ScopeGlobal
	ScopeWorkspace             = cfgmodel.ScopeWorkspace
	SettingsAuthorityGuarded   = cfgmodel.SettingsAuthorityGuarded
	SettingsAuthorityTrustedUI = cfgmodel.SettingsAuthorityTrustedUI
)

// MCPView is one published store snapshot as seen by the MCP component.
type MCPView interface {
	Config() *cfgmodel.Config
	MCPRevisions() map[string]uint64
	MCPInputs() map[string][32]byte
	Resolver() envresolve.VariableResolver
	ResolverRevision() uint64
	Snapshots() map[string]docread.FileSnapshot
	WorkspacePath() string
	Generation() uint64
}

// MutationScope is handed to PublishMutation callbacks with publishMu held:
// Current reads the snapshot under the lock, Publish clones it, replaces the
// given fields, and publishes the next generation.
type MutationScope interface {
	Current() MCPView
	Publish(next MCPNext)
}

// MCPNext carries the snapshot fields an MCP mutation replaces; every field
// left nil/false is carried over from the current snapshot.
type MCPNext struct {
	Config          *cfgmodel.Config
	MCPRevisions    map[string]uint64
	MCPInputs       map[string][32]byte
	ResolverDynamic bool
	Snapshots       map[string]docread.FileSnapshot
}

// mutationNext returns the empty replacement set for one publish.
func mutationNext() MCPNext { return MCPNext{} }

// LockTarget is one resolved physical config write destination as seen by
// the MCP lock machinery. The core port converts it to/from its private
// configWriteTarget.
type LockTarget struct {
	Path         string
	SelectedPath string
	LockPath     string
	Expected     docread.Fingerprint
	Owner        int
	Enforce      bool
}

// DiskWriter is the write-path seam handed to DiskWrite callbacks with
// diskWriteMu held.
type DiskWriter interface {
	// ResolveWriteTargets pins the physical spelling and sidecar for each
	// path (and runs the resolve-target hook) before the locks are taken.
	ResolveWriteTargets(paths []string) ([]LockTarget, error)
	// VerifyTargetBindingAfterLock rereads the destination after the
	// sidecar lock is acquired.
	VerifyTargetBindingAfterLock(target LockTarget) error
	// VerifyTargetBinding confirms the pre-lock selection still matches the
	// post-lock reread (readErr carries the read failure, if any).
	VerifyTargetBinding(target LockTarget, actual docread.Fingerprint, readErr error) error
}

// Port exposes exactly the core ConfigStore primitives the MCP component
// needs. It never lets mcpstore import the root config package.
type Port interface {
	// ViewSnap samples the published snapshot exactly once per callback.
	ViewSnap(fn func(MCPView) error) error
	// PublishMutation runs fn with publishMu held.
	PublishMutation(fn func(MutationScope) error) error
	// MutateConfig applies a copy-on-write config mutation under publishMu.
	MutateConfig(mutate func(*cfgmodel.Config))
	// ScopePath resolves the logical slot to its physical path.
	ScopePath(scope cfgmodel.Scope) (string, error)
	GlobalDataPath() string
	WorkingDir() string
	// SystemConfigPath returns the (possibly overridden) system config path.
	SystemConfigPath() string
	// WorkingDirOwner computes (once) the working-directory config owner and
	// reports whether a working directory is set.
	WorkingDirOwner() (owner int, hasWorkingDir bool, err error)
	// DiskWrite runs fn with diskWriteMu held.
	DiskWrite(fn func(DiskWriter) error) error
	// NoteInitialLoadWrite requires diskWriteMu to be held and may be called
	// only from inside a WithFileWriteLock callback. It must not call
	// PublishMutation or re-enter DiskWrite.
	NoteInitialLoadWrite(path string, data []byte)
	// SettingsGuard returns the settings component's write guard.
	SettingsGuard() *settings.Store
	// CommitDiskWrite fences reloads: bumps the disk write generation after
	// a committed config write. The caller must already hold diskWriteMu
	// (i.e. run inside DiskWrite).
	CommitDiskWrite()
}

// Store is the MCP persistence component over one core store port.
type Store struct {
	port Port

	// mcpUncertainty owns uncertainty raised by a durable MCP mutation. The
	// state is deliberately outside the replaceable config snapshot so
	// reloads cannot lose it before their successful publication boundary.
	mcpUncertainty mcpUncertaintyState
}

// NewStore builds the MCP component over the given core port.
func NewStore(p Port) *Store { return &Store{port: p} }

// MCPAdmissionSnapshot captures the MCP definition and resolver revisions
// from one immutable store snapshot.
type MCPAdmissionSnapshot struct {
	Config           *Config
	Resolver         VariableResolver
	MCPConfig        MCPConfig
	Exists           bool
	MCPInput         [32]byte
	HasMCPInput      bool
	MCPRevision      uint64
	ResolverRevision uint64
	Generation       uint64
}

// SnapshotMCPAdmission captures the MCP definition and resolver revisions
// from one immutable store snapshot.
func (s *Store) SnapshotMCPAdmission(name string) MCPAdmissionSnapshot {
	var result MCPAdmissionSnapshot
	_ = s.port.ViewSnap(func(view MCPView) error {
		config := view.Config()
		result = MCPAdmissionSnapshot{
			Config:           config,
			Resolver:         view.Resolver(),
			MCPRevision:      view.MCPRevisions()[name],
			ResolverRevision: view.ResolverRevision(),
			Generation:       view.Generation(),
		}
		result.MCPInput, result.HasMCPInput = view.MCPInputs()[name]
		if config != nil {
			result.MCPConfig, result.Exists = config.MCP[name]
			if result.Exists {
				result.MCPConfig = cloneMCPConfig(result.MCPConfig)
			}
		}
		return nil
	})
	return result
}

// currentMCPInputs reads the published MCP input fingerprints.
func (s *Store) currentMCPInputs() map[string][32]byte {
	var inputs map[string][32]byte
	_ = s.port.ViewSnap(func(view MCPView) error {
		inputs = view.MCPInputs()
		return nil
	})
	return inputs
}

// currentGeneration reads the published snapshot generation.
func (s *Store) currentGeneration() uint64 {
	var generation uint64
	_ = s.port.ViewSnap(func(view MCPView) error {
		generation = view.Generation()
		return nil
	})
	return generation
}

// workspacePathValue reads the published workspace path.
func (s *Store) workspacePathValue() string {
	var path string
	_ = s.port.ViewSnap(func(view MCPView) error {
		path = view.WorkspacePath()
		return nil
	})
	return path
}

// UpdateMCP applies a copy-on-write mutation to one MCP server configuration.
// It never mutates a published Config or any of its nested MCP maps.
func (s *Store) UpdateMCP(name string, mutate func(*MCPConfig)) (MCPConfig, bool) {
	var updated MCPConfig
	var ok bool
	_ = s.port.PublishMutation(func(scope MutationScope) error {
		cur := scope.Current()
		config := cur.Config()
		if config == nil || config.MCP == nil {
			return nil
		}
		current, exists := config.MCP[name]
		if !exists {
			return nil
		}
		updated = cloneMCPConfig(current)
		mutate(&updated)
		next := mutationNext()
		cfgCopy := *config
		cfgCopy.MCP = maps.Clone(config.MCP)
		cfgCopy.MCP[name] = cloneMCPConfig(updated)
		next.Config = &cfgCopy
		next.MCPRevisions = mcpRevisionDiff(cur.MCPRevisions(), config, &cfgCopy, cur.MCPInputs(), next.MCPInputs, name)
		next.ResolverDynamic = configHasDynamicMCPResolution(&cfgCopy)
		scope.Publish(next)
		ok = true
		return nil
	})
	return updated, ok
}

// SetMCPDisabled updates one MCP server's disabled flag through the
// copy-on-write publication path and returns the detached new value.
func (s *Store) SetMCPDisabled(name string, disabled bool) (MCPConfig, bool) {
	return s.UpdateMCP(name, func(m *MCPConfig) { m.Disabled = disabled })
}

// AddMCP publishes a new MCP server configuration if name is unused.
func (s *Store) AddMCP(name string, mcpConfig MCPConfig) bool {
	var added bool
	_ = s.port.PublishMutation(func(scope MutationScope) error {
		cur := scope.Current()
		config := cur.Config()
		if config == nil {
			return nil
		}
		if _, exists := config.MCP[name]; exists {
			return nil
		}
		next := mutationNext()
		cfgCopy := *config
		cfgCopy.MCP = maps.Clone(config.MCP)
		if cfgCopy.MCP == nil {
			cfgCopy.MCP = make(MCPs)
		}
		cfgCopy.MCP[name] = cloneMCPConfig(mcpConfig)
		next.Config = &cfgCopy
		next.MCPRevisions = mcpRevisionDiff(cur.MCPRevisions(), config, &cfgCopy, cur.MCPInputs(), next.MCPInputs)
		next.ResolverDynamic = configHasDynamicMCPResolution(&cfgCopy)
		scope.Publish(next)
		added = true
		return nil
	})
	return added
}

// RemoveMCP removes one MCP server and returns its detached configuration.
func (s *Store) RemoveMCP(name string) (MCPConfig, bool) {
	var removed MCPConfig
	var ok bool
	_ = s.port.PublishMutation(func(scope MutationScope) error {
		cur := scope.Current()
		config := cur.Config()
		if config == nil {
			return nil
		}
		current, exists := config.MCP[name]
		if !exists {
			return nil
		}
		next := mutationNext()
		cfgCopy := *config
		cfgCopy.MCP = maps.Clone(config.MCP)
		delete(cfgCopy.MCP, name)
		next.Config = &cfgCopy
		next.MCPRevisions = mcpRevisionDiff(cur.MCPRevisions(), config, &cfgCopy, cur.MCPInputs(), next.MCPInputs)
		next.ResolverDynamic = configHasDynamicMCPResolution(&cfgCopy)
		scope.Publish(next)
		removed = cloneMCPConfig(current)
		ok = true
		return nil
	})
	return removed, ok
}

// RemoveMCPIfCurrent removes name only when the in-memory snapshot still has
// the expected config at the expected MCP revision.
func (s *Store) RemoveMCPIfCurrent(name string, expected MCPConfig, revision uint64) (MCPConfig, bool) {
	var removed MCPConfig
	var ok bool
	_ = s.port.PublishMutation(func(scope MutationScope) error {
		cur := scope.Current()
		config := cur.Config()
		if config == nil || revision != 0 && cur.MCPRevisions()[name] != revision {
			return nil
		}
		current, exists := config.MCP[name]
		if !exists || !reflect.DeepEqual(current, expected) {
			return nil
		}
		next := mutationNext()
		cfgCopy := *config
		cfgCopy.MCP = maps.Clone(config.MCP)
		delete(cfgCopy.MCP, name)
		next.Config = &cfgCopy
		next.MCPRevisions = mcpRevisionDiff(cur.MCPRevisions(), config, &cfgCopy, cur.MCPInputs(), next.MCPInputs)
		next.ResolverDynamic = configHasDynamicMCPResolution(&cfgCopy)
		scope.Publish(next)
		removed = cloneMCPConfig(current)
		ok = true
		return nil
	})
	return removed, ok
}

// cloneMCPConfig is a byte-identical local copy of the root helper
// (internal/config/store.go cloneMCPConfig) that moved with the MCP
// component; the root keeps its own copy for MCPConfig.
func cloneMCPConfig(m MCPConfig) MCPConfig {
	m.Env = maps.Clone(m.Env)
	m.Args = slices.Clone(m.Args)
	m.DisabledTools = slices.Clone(m.DisabledTools)
	m.EnabledTools = slices.Clone(m.EnabledTools)
	m.Headers = maps.Clone(m.Headers)
	return m
}

// mcpUncertaintyState tracks per-name fence versions raised by uncertain
// commits (moved from internal/config/mcp_uncertainty.go unchanged except
// the receiver).
type mcpUncertaintyState struct {
	mu       sync.RWMutex
	next     uint64
	versions map[string]uint64
}

// MarkMCPUncertain raises the fail-closed fence for name and returns its
// version. A subsequent successful reload of this exact store clears it.
func (s *Store) MarkMCPUncertain(name string) uint64 {
	if s == nil {
		return 0
	}
	s.mcpUncertainty.mu.Lock()
	defer s.mcpUncertainty.mu.Unlock()
	if s.mcpUncertainty.versions == nil {
		s.mcpUncertainty.versions = make(map[string]uint64)
	}
	s.mcpUncertainty.next++
	s.mcpUncertainty.versions[name] = s.mcpUncertainty.next
	return s.mcpUncertainty.next
}

// MCPUncertaintyVersion reports the current fence version for name.
func (s *Store) MCPUncertaintyVersion(name string) (uint64, bool) {
	if s == nil {
		return 0, false
	}
	s.mcpUncertainty.mu.RLock()
	version, ok := s.mcpUncertainty.versions[name]
	s.mcpUncertainty.mu.RUnlock()
	return version, ok
}

// MCPUncertaintyVersions returns a snapshot of the requested fences. With no
// names it returns every fence.
func (s *Store) MCPUncertaintyVersions(names ...string) map[string]uint64 {
	result := make(map[string]uint64)
	if s == nil {
		return result
	}
	s.mcpUncertainty.mu.RLock()
	defer s.mcpUncertainty.mu.RUnlock()
	if len(names) == 0 {
		for name, version := range s.mcpUncertainty.versions {
			result[name] = version
		}
		return result
	}
	for _, name := range names {
		if version, ok := s.mcpUncertainty.versions[name]; ok {
			result[name] = version
		}
	}
	return result
}

// ClearUncertainty clears only fences observed before the reload began
// (root back-call from store_reload.go; was clearMCPUncertainty).
// Fences raised while a reload is in flight remain for the next reconciliation.
func (s *Store) ClearUncertainty(expected map[string]uint64) {
	if s == nil {
		return
	}
	s.mcpUncertainty.mu.Lock()
	for name, version := range expected {
		if current, ok := s.mcpUncertainty.versions[name]; ok && current == version {
			delete(s.mcpUncertainty.versions, name)
		}
	}
	s.mcpUncertainty.mu.Unlock()
}

// OwnerPolicyFor resolves the trust policy for one config path (root
// back-call from store_write.go resolveConfigWriteTarget; was
// mcpOwnerPolicy; the workingDirOwnerOnce/owner/err tail moved behind
// Port.WorkingDirOwner).
func (s *Store) OwnerPolicyFor(path string) (int, bool, error) {
	if strings.EqualFold(filepath.Base(path), ".mcp.json") {
		if strings.EqualFold(
			docread.NormalizeDiscoveryPath(path),
			docread.NormalizeDiscoveryPath(filepath.Join(home.Dir(), ".claude", ".mcp.json")),
		) {
			return fileid.HomeConfigOwner(), true, nil
		}
		owner, hasWorkingDir, err := s.port.WorkingDirOwner()
		if err != nil {
			return 0, false, err
		}
		return owner, hasWorkingDir, nil
	}
	canonical := docread.NormalizeReloadPath(path)
	if systemPath := s.systemConfigPathValue(); systemPath != "" && canonical == docread.NormalizeReloadPath(systemPath) {
		return fileid.SystemConfigOwner(), true, nil
	}
	if docread.MatchesLogicalSlot(canonical, datadir.GlobalConfig()) || docread.MatchesLogicalSlot(canonical, s.port.GlobalDataPath()) {
		return fileid.HomeConfigOwner(), true, nil
	}
	if canonical == docread.NormalizeReloadPath(filepath.Join(home.Dir(), ".claude", ".mcp.json")) {
		return fileid.HomeConfigOwner(), true, nil
	}
	owner, hasWorkingDir, err := s.port.WorkingDirOwner()
	if err != nil {
		return 0, false, err
	}
	return owner, hasWorkingDir, nil
}

// mcpOwnerPolicy is the component-internal name for OwnerPolicyFor, kept so
// moved method bodies stay byte-identical.
func (s *Store) mcpOwnerPolicy(path string) (int, bool, error) { return s.OwnerPolicyFor(path) }

// systemConfigPathValue returns the effective system config path (was
// systemConfigPathValue on ConfigStore; the override field moved behind
// Port.SystemConfigPath).
func (s *Store) systemConfigPathValue() string {
	if override := s.port.SystemConfigPath(); override != "" {
		return override
	}
	return datadir.SystemConfig()
}

// mcpRevisionDiff, mcpNames and mcpConfig are byte-identical local copies of
// the root helpers (internal/config/mcpdocs_policy.go:62,110 and
// store_reload.go:435) still needed to advance revisions on publish (closure
// copies; the root keeps the originals for reload).
func mcpRevisionDiff(current map[string]uint64, previous, next *Config, previousInputs, nextInputs map[string][32]byte, force ...string) map[string]uint64 {
	result := maps.Clone(current)
	if result == nil {
		result = make(map[string]uint64)
	}
	names := make(map[string]struct{})
	for name := range mcpNames(previous) {
		names[name] = struct{}{}
	}
	for name := range mcpNames(next) {
		names[name] = struct{}{}
	}
	for name := range previousInputs {
		names[name] = struct{}{}
	}
	for name := range nextInputs {
		names[name] = struct{}{}
	}
	for _, name := range force {
		if name != "" {
			names[name] = struct{}{}
		}
	}
	forced := make(map[string]struct{}, len(force))
	for _, name := range force {
		forced[name] = struct{}{}
	}
	for name := range names {
		previousConfig, previousExists := mcpConfig(previous, name)
		nextConfig, nextExists := mcpConfig(next, name)
		previousInput, previousHasInput := previousInputs[name]
		nextInput, nextHasInput := nextInputs[name]
		if _, ok := forced[name]; ok || previousExists != nextExists ||
			previousExists && !reflect.DeepEqual(previousConfig, nextConfig) ||
			previousHasInput != nextHasInput ||
			previousHasInput && nextHasInput && previousInput != nextInput {
			result[name]++
		}
	}
	return result
}

func mcpNames(cfg *Config) map[string]cfgmodel.MCPConfig {
	if cfg == nil {
		return nil
	}
	return cfg.MCP
}

func mcpConfig(cfg *Config, name string) (MCPConfig, bool) {
	if cfg == nil {
		return MCPConfig{}, false
	}
	value, ok := cfg.MCP[name]
	return value, ok
}

// configHasDynamicMCPResolution, mcpConfigHasDynamicResolution and
// hasCommandSubstitution are byte-identical local copies of the root
// closures (internal/config/cfgmodel_locals.go:167-204) needed by the
// publish path.
func configHasDynamicMCPResolution(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	for _, m := range cfg.MCP {
		if mcpConfigHasDynamicResolution(m) {
			return true
		}
	}
	return false
}

func mcpConfigHasDynamicResolution(m MCPConfig) bool {
	if shell.HasCommandSubstitution(m.Command) || shell.HasCommandSubstitution(m.URL) {
		return true
	}
	for _, value := range m.Env {
		if shell.HasCommandSubstitution(value) {
			return true
		}
	}
	for _, value := range m.Args {
		if shell.HasCommandSubstitution(value) {
			return true
		}
	}
	for _, value := range m.Headers {
		if shell.HasCommandSubstitution(value) {
			return true
		}
	}
	return false
}
