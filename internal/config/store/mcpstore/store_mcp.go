package mcpstore

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/store/settings"
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
