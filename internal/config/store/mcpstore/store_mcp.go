package mcpstore

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
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
)

var (
	ErrMCPNotFound = errors.New("MCP server not found")
	ErrMCPExternal = errors.New("MCP server is from .mcp.json and is not writable")
	// ErrMCPAmbiguous remains an alias for compatibility with older callers.
	ErrMCPAmbiguous        = ErrMCPExternal
	ErrMCPUnwritableOrigin = errors.New("MCP server has no writable config scope")
	ErrMCPTargetExists     = errors.New("MCP server target already exists")
	ErrMCPStale            = errors.New("MCP server configuration changed while preparing mutation")
	ErrMCPCommitUncertain  = errors.New("MCP config commit outcome is uncertain")
	ErrMCPMutationStale    = errors.New("MCP mutation result is stale")
)

// ResolveMCPWritableScope resolves the effective owner at a fresh disk
// transaction boundary. It never trusts a previously loaded snapshot for a
// scope decision, so a concurrent promotion cannot redirect a write.
func (s *Store) ResolveMCPWritableScope(name string) (Scope, error) {
	var origin MCPOrigin
	var cfg MCPConfig
	var exists bool
	err := s.withMCPWriteLocks(func(files *LockedFiles) error {
		eval, err := s.EvaluateMCPFiles(files)
		if err != nil {
			return err
		}
		cfg, exists = eval.Configs[name]
		origin = eval.Origins[name]
		return nil
	})
	if err != nil {
		return ScopeGlobal, err
	}
	if !exists {
		return ScopeGlobal, fmt.Errorf("%w: %q", ErrMCPNotFound, name)
	}
	if cfg.Source == MCPSourceExternal || origin.Kind == MCPOriginExternal {
		return ScopeGlobal, fmt.Errorf("%w: %q", ErrMCPExternal, name)
	}
	if !origin.Writable {
		return ScopeGlobal, fmt.Errorf("%w: %q is defined in %s", ErrMCPUnwritableOrigin, name, origin.Path)
	}
	return origin.Scope, nil
}

func (s *Store) PersistMCPConfig(scope Scope, name string, value MCPConfig) error {
	_, err := s.PersistMCPConfigResult(scope, name, value)
	return err
}

// The PersistMCP*WithAuthority methods below are the persistence seams the
// MCP Owner's trusted-UI mutations call: each delegates to the identical
// guarded transaction machinery (the same ordered two-file write locks,
// CAS/atomic commit, scope ownership checks, and audit events), with one
// difference — the presented authority lifts ONLY the password/settings-
// lock veto inside that single transaction. Every other caller must keep
// using the guarded variants.

func (s *Store) PersistMCPConfigResultWithAuthority(scope Scope, name string, value MCPConfig, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	return s.mutateMCPWithMode("add", scope, name, name, value, nil, false, authority)
}

// PersistMCPConfigExact adds a literal server entry to the selected scope.
// Unlike the runtime lifecycle API, it deliberately ignores higher-priority
// definitions in other files; this is the API used by exact-scope CLI edits.
func (s *Store) PersistMCPConfigExact(scope Scope, name string, value MCPConfig) error {
	_, err := s.mutateMCPExact("add", scope, name, name, value, nil)
	return err
}

func (s *Store) PersistMCPConfigResult(scope Scope, name string, value MCPConfig) (MCPMutationResult, error) {
	return s.PersistMCPConfigResultWithAuthority(scope, name, value, SettingsAuthorityGuarded)
}

func (s *Store) PersistRemoveMCPConfig(scope Scope, name string) error {
	_, err := s.PersistRemoveMCPConfigResult(scope, name)
	return err
}

// PersistRemoveMCPConfigExact removes only the literal entry in the selected
// scope, even when another scope shadows it in the effective configuration.
func (s *Store) PersistRemoveMCPConfigExact(scope Scope, name string) error {
	_, err := s.mutateMCPExact("remove", scope, name, name, MCPConfig{}, nil)
	return err
}

func (s *Store) PersistRemoveMCPConfigResult(scope Scope, name string) (MCPMutationResult, error) {
	return s.PersistRemoveMCPConfigResultWithAuthority(scope, name, SettingsAuthorityGuarded)
}

func (s *Store) PersistRemoveMCPConfigResultWithAuthority(scope Scope, name string, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	return s.mutateMCPWithMode("remove", scope, name, name, MCPConfig{}, nil, false, authority)
}

// PersistRemovePendingMCPConfigResult conditionally completes a pending MCP
// add without ever writing its in-memory definition. It succeeds only while
// the server is absent from the durable configuration; an existing durable
// definition is a collision and is left untouched.
func (s *Store) PersistRemovePendingMCPConfigResult(scope Scope, name string) (MCPMutationResult, error) {
	return s.PersistRemovePendingMCPConfigResultWithAuthority(scope, name, SettingsAuthorityGuarded)
}

func (s *Store) PersistRemovePendingMCPConfigResultWithAuthority(scope Scope, name string, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	return s.mutatePendingRemoveMCPWithAuthority(scope, name, authority)
}

// PersistRemovePendingMCPConfig is the error-only form of
// PersistRemovePendingMCPConfigResult.
func (s *Store) PersistRemovePendingMCPConfig(scope Scope, name string) error {
	_, err := s.PersistRemovePendingMCPConfigResult(scope, name)
	return err
}

func (s *Store) PersistMCPDisabledOverride(scope Scope, name string, disabled bool) error {
	_, err := s.PersistMCPDisabledOverrideResult(scope, name, disabled)
	return err
}

func (s *Store) PersistMCPDisabledOverrideResult(scope Scope, name string, disabled bool) (MCPMutationResult, error) {
	return s.PersistMCPDisabledOverrideResultWithAuthority(scope, name, disabled, SettingsAuthorityGuarded)
}

func (s *Store) PersistMCPDisabledOverrideResultWithAuthority(scope Scope, name string, disabled bool, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	return s.mutateMCPWithMode("disable", scope, name, name, MCPConfig{}, &disabled, false, authority)
}

// PersistMCPEnableRollbackResult conditionally disables the exact definition
// published by an Enable mutation. It is safe for both ordinary definitions
// and pending global adds.
func (s *Store) PersistMCPEnableRollbackResult(scope Scope, name string, token MCPMutationResult) (MCPMutationResult, error) {
	return s.PersistMCPEnableRollbackResultWithAuthority(scope, name, token, SettingsAuthorityGuarded)
}

func (s *Store) PersistMCPEnableRollbackResultWithAuthority(scope Scope, name string, token MCPMutationResult, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	return s.mutateMCPEnableRollbackWithAuthority(scope, name, token, authority)
}

// PersistMCPDisabledOverrideExact changes only the selected scope. It may
// update a shadowed literal entry and may create a workspace overlay for an
// external server, but it never redirects the write to another scope.
func (s *Store) PersistMCPDisabledOverrideExact(scope Scope, name string, disabled bool) error {
	_, err := s.PersistMCPDisabledOverrideExactResult(scope, name, disabled)
	return err
}

func (s *Store) PersistMCPDisabledOverrideExactResult(scope Scope, name string, disabled bool) (MCPMutationResult, error) {
	return s.mutateMCPExact("disable", scope, name, name, MCPConfig{}, &disabled)
}

// PersistMCPDisabledOverrideAtScope changes only the selected scope.
func (s *Store) PersistMCPDisabledOverrideAtScope(scope Scope, name string, disabled bool) error {
	return s.PersistMCPDisabledOverrideExact(scope, name, disabled)
}

// PersistMCPDisabledOverrideInScope changes only the selected scope.
func (s *Store) PersistMCPDisabledOverrideInScope(scope Scope, name string, disabled bool) error {
	return s.PersistMCPDisabledOverrideExact(scope, name, disabled)
}

func (s *Store) PersistReplaceMCP(oldName, newName string, value MCPConfig) error {
	_, err := s.PersistReplaceMCPResult(ScopeGlobal, oldName, newName, value)
	return err
}

func (s *Store) PersistReplaceMCPResult(scope Scope, oldName, newName string, value MCPConfig) (MCPMutationResult, error) {
	return s.PersistReplaceMCPResultWithAuthority(scope, oldName, newName, value, SettingsAuthorityGuarded)
}

func (s *Store) PersistReplaceMCPResultWithAuthority(scope Scope, oldName, newName string, value MCPConfig, authority SettingsWriteAuthority) (MCPMutationResult, error) {
	return s.mutateMCPWithMode("replace", scope, oldName, newName, value, nil, false, authority)
}

func (s *Store) PersistReplaceMCPInScope(scope Scope, oldName, newName string, value MCPConfig) error {
	_, err := s.PersistReplaceMCPResult(scope, oldName, newName, value)
	return err
}

func (s *Store) PersistReplaceMCPAtScope(scope Scope, oldName, newName string, value MCPConfig) error {
	return s.PersistReplaceMCPInScope(scope, oldName, newName, value)
}

// PersistMCPFields is the low-level literal-key field editor retained for
// non-lifecycle callers. It still takes the same ordered two-file transaction
// locks as lifecycle mutations.
func (s *Store) PersistMCPFields(scope Scope, name string, fields map[string]any) error {
	path, err := s.port.ScopePath(scope)
	if err != nil {
		return err
	}
	path = docread.NormalizeReloadPath(path)
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var effective MCPConfig
	var effectiveExists bool
	var committed map[string]docread.Fingerprint
	var committedInputs map[string][32]byte
	var audit settingsWriteAudit
	err = s.port.PublishMutation(func(pub MutationScope) error {
		err = s.withMCPWriteLocks(func(files *LockedFiles) error {
			before, evalErr := s.EvaluateMCPFiles(files)
			if evalErr != nil {
				return evalErr
			}
			if err := files.validateMutableTopology(); err != nil {
				return err
			}
			_, oldOK := before.Configs[name]
			if err := validateMCPMutation("set", scope, name, name, oldOK, before.Origins[name], before.Configs); err != nil {
				return err
			}
			if err := UpdateMCPFile(files, path, name, func(entry map[string]any) error {
				for _, key := range keys {
					entry[key] = fields[key]
				}
				return nil
			}); err != nil {
				return err
			}
			if err := s.verifyMCPReadOnlyInputs(before.Fingerprints, path); err != nil {
				return err
			}
			after, err := s.EvaluateMCPFiles(files)
			if err != nil {
				return err
			}
			effectiveExists, effective, _ = afterValue(after, name)
			committedInputs = reconcileMCPInputs(s.currentMCPInputs(), after.MCPInputs, name)
			committed = committedMCPFingerprints(files)
			writeErr := s.writeMCPFileChanges(files, &audit, SettingsAuthorityGuarded)
			committed = committedMCPFingerprints(files)
			if writeErr != nil {
				return writeErr
			}
			return nil
		})
		if err == nil || MCPCommitWasReconciled(err) {
			// The selected file may be shadowed, so publish the effective value
			// only after the exact file mutation has committed. A later reload
			// remains authoritative for all other fields.
			s.publishMCPValueAndStalenessLocked(pub, name, effective, effectiveExists, committed, committedInputs)
		}
		return nil
	})
	auditErr := err
	if MCPCommitWasReconciled(err) {
		auditErr = nil
	}
	if audit.event != nil {
		settingsFinalizeEvent(audit.event, audit.refusedBy, auditErr)
		settingsEmitEvent(*audit.event)
	}
	if err != nil {
		return err
	}
	return nil
}

// PersistMCPFieldsExact updates only the literal entry in the selected file.
// It does not resolve or validate the effective owner in another scope.
func (s *Store) PersistMCPFieldsExact(scope Scope, name string, fields map[string]any) error {
	path, err := s.port.ScopePath(scope)
	if err != nil {
		return err
	}
	path = docread.NormalizeReloadPath(path)
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var effective MCPConfig
	var effectiveExists bool
	var committed map[string]docread.Fingerprint
	var committedInputs map[string][32]byte
	var audit settingsWriteAudit
	err = s.port.PublishMutation(func(pub MutationScope) error {
		err = s.withMCPWriteLocks(func(files *LockedFiles) error {
			before, evalErr := s.EvaluateMCPFiles(files)
			if evalErr != nil {
				return evalErr
			}
			if err := files.validateMutableTopology(); err != nil {
				return err
			}
			if !literalMCPEntryExists(files.MCPData(path), name) {
				return fmt.Errorf("%w: %q", ErrMCPNotFound, name)
			}
			if err := UpdateMCPFile(files, path, name, func(entry map[string]any) error {
				for _, key := range keys {
					entry[key] = fields[key]
				}
				return nil
			}); err != nil {
				return err
			}
			if err := s.verifyMCPReadOnlyInputs(before.Fingerprints, path); err != nil {
				return err
			}
			after, err := s.EvaluateMCPFiles(files)
			if err != nil {
				return err
			}
			effectiveExists, effective, _ = afterValue(after, name)
			committedInputs = reconcileMCPInputs(s.currentMCPInputs(), after.MCPInputs, name)
			committed = committedMCPFingerprints(files)
			writeErr := s.writeMCPFileChanges(files, &audit, SettingsAuthorityGuarded)
			committed = committedMCPFingerprints(files)
			if writeErr != nil {
				return writeErr
			}
			return nil
		})
		if err == nil || MCPCommitWasReconciled(err) {
			s.publishMCPValueAndStalenessLocked(pub, name, effective, effectiveExists, committed, committedInputs)
		}
		return nil
	})
	auditErr := err
	if MCPCommitWasReconciled(err) {
		auditErr = nil
	}
	if audit.event != nil {
		settingsFinalizeEvent(audit.event, audit.refusedBy, auditErr)
		settingsEmitEvent(*audit.event)
	}
	return err
}

// AtScope aliases make the exact-file intent explicit to callers that prefer
// the older lifecycle method naming convention.
func (s *Store) PersistMCPConfigAtScope(scope Scope, name string, value MCPConfig) error {
	return s.PersistMCPConfigExact(scope, name, value)
}

func (s *Store) PersistRemoveMCPConfigAtScope(scope Scope, name string) error {
	return s.PersistRemoveMCPConfigExact(scope, name)
}

func (s *Store) PersistMCPFieldsAtScope(scope Scope, name string, fields map[string]any) error {
	return s.PersistMCPFieldsExact(scope, name, fields)
}

func (s *Store) PersistMCPConfigInScope(scope Scope, name string, value MCPConfig) error {
	return s.PersistMCPConfigExact(scope, name, value)
}

func (s *Store) PersistRemoveMCPConfigInScope(scope Scope, name string) error {
	return s.PersistRemoveMCPConfigExact(scope, name)
}

func (s *Store) PersistMCPFieldsInScope(scope Scope, name string, fields map[string]any) error {
	return s.PersistMCPFieldsExact(scope, name, fields)
}

func (s *Store) publishMCPValueAndStalenessLocked(scope MutationScope, name string, value MCPConfig, exists bool, committed map[string]docread.Fingerprint, committedInputs map[string][32]byte) {
	cur := scope.Current()
	if cur.Config() == nil {
		return
	}
	next := mutationNext()
	cfg := *cur.Config()
	cfg.MCP = maps.Clone(cur.Config().MCP)
	if cfg.MCP == nil {
		cfg.MCP = make(MCPs)
	}
	if exists {
		cfg.MCP[name] = cloneMCPConfig(value)
	} else {
		delete(cfg.MCP, name)
	}
	next.Config = &cfg
	if committedInputs != nil {
		next.MCPInputs = maps.Clone(committedInputs)
	}
	next.MCPRevisions = mcpRevisionDiff(cur.MCPRevisions(), cur.Config(), next.Config, cur.MCPInputs(), next.MCPInputs)
	next.ResolverDynamic = configHasDynamicMCPResolution(next.Config)
	if len(committed) > 0 {
		if cur.Snapshots() == nil {
			next.Snapshots = make(map[string]fileSnapshot)
		} else {
			next.Snapshots = maps.Clone(cur.Snapshots())
		}
		for path, fingerprint := range committed {
			key := docread.NormalizeDiscoveryPath(path)
			snapshot, ok := next.Snapshots[key]
			if !ok {
				continue
			}
			snapshot = docread.NewFileSnapshot(key, fingerprint)
			next.Snapshots[key] = snapshot
		}
	}
	scope.Publish(next)
}

// settingsWriteAudit is a byte-identical local copy of the root audit
// carrier (internal/config/facade.go:557) retyped over the settings
// component's SettingsEvent.
type settingsWriteAudit struct {
	event     *settings.SettingsEvent
	refusedBy string
}

// settingsFinalizeEvent and settingsEmitEvent forward to the settings
// component (the root store_write.go forwarders do the same).
func settingsFinalizeEvent(ev *settings.SettingsEvent, refusedBy string, err error) {
	settings.FinalizeSettingsEvent(ev, refusedBy, err)
}

func settingsEmitEvent(ev settings.SettingsEvent) { settings.EmitSettingsEvent(ev) }

// Uncertainty fences and per-path owner policy (moved from mcpstore.go to keep the directory at 7 files).
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

// Moved from mcpstore.go (file deleted to keep the directory at 7 files).
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
