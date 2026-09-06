package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

var (
	ErrMCPNotFound         = errors.New("MCP server not found")
	ErrMCPExternal         = errors.New("MCP server is from .mcp.json and is not writable")
	ErrMCPAmbiguous        = ErrMCPExternal
	ErrMCPUnwritableOrigin = errors.New("MCP server has no writable config scope")
	ErrMCPTargetExists     = errors.New("MCP server target already exists")
	ErrMCPStale            = errors.New("MCP server configuration changed while preparing mutation")
)

// MCPMutationResult describes the effective configuration on both sides of a
// durable MCP mutation. NewConfig/NewOrigin may describe a lower-priority
// definition revealed by removing or replacing the old one.
type MCPMutationResult struct {
	Operation      string
	OldName        string
	NewName        string
	OldExists      bool
	NewExists      bool
	OldConfig      MCPConfig
	NewConfig      MCPConfig
	OldOrigin      MCPOrigin
	NewOrigin      MCPOrigin
	FallbackExists bool
	FallbackConfig MCPConfig
	FallbackOrigin MCPOrigin
}

// ResolveMCPWritableScope resolves the effective owner at a fresh disk
// transaction boundary. It never trusts a previously loaded snapshot for a
// scope decision, so a concurrent promotion cannot redirect a write.
func (s *ConfigStore) ResolveMCPWritableScope(name string) (Scope, error) {
	var origin MCPOrigin
	var cfg MCPConfig
	var exists bool
	err := s.withMCPWriteLocks(func(files *mcpLockedFiles) error {
		eval, err := s.evaluateMCPFiles(files)
		if err != nil {
			return err
		}
		cfg, exists = eval.configs[name]
		origin = eval.origins[name]
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

func (s *ConfigStore) PersistMCPConfig(scope Scope, name string, value MCPConfig) error {
	_, err := s.PersistMCPConfigResult(scope, name, value)
	return err
}

// PersistMCPConfigExact adds a literal server entry to the selected scope.
// Unlike the runtime lifecycle API, it deliberately ignores higher-priority
// definitions in other files; this is the API used by exact-scope CLI edits.
func (s *ConfigStore) PersistMCPConfigExact(scope Scope, name string, value MCPConfig) error {
	_, err := s.mutateMCPExact("add", scope, name, name, value, nil)
	return err
}

func (s *ConfigStore) PersistMCPConfigResult(scope Scope, name string, value MCPConfig) (MCPMutationResult, error) {
	return s.mutateMCP("add", scope, name, name, value, nil)
}

func (s *ConfigStore) PersistRemoveMCPConfig(scope Scope, name string) error {
	_, err := s.PersistRemoveMCPConfigResult(scope, name)
	return err
}

// PersistRemoveMCPConfigExact removes only the literal entry in the selected
// scope, even when another scope shadows it in the effective configuration.
func (s *ConfigStore) PersistRemoveMCPConfigExact(scope Scope, name string) error {
	_, err := s.mutateMCPExact("remove", scope, name, name, MCPConfig{}, nil)
	return err
}

func (s *ConfigStore) PersistRemoveMCPConfigResult(scope Scope, name string) (MCPMutationResult, error) {
	return s.mutateMCP("remove", scope, name, name, MCPConfig{}, nil)
}

// PersistRemovePendingMCPConfigResult conditionally completes a pending MCP
// add without ever writing its in-memory definition. It succeeds only while
// the server is absent from the durable configuration; an existing durable
// definition is a collision and is left untouched.
func (s *ConfigStore) PersistRemovePendingMCPConfigResult(scope Scope, name string) (MCPMutationResult, error) {
	return s.mutatePendingRemoveMCP(scope, name)
}

// PersistRemovePendingMCPConfig is the error-only form of
// PersistRemovePendingMCPConfigResult.
func (s *ConfigStore) PersistRemovePendingMCPConfig(scope Scope, name string) error {
	_, err := s.PersistRemovePendingMCPConfigResult(scope, name)
	return err
}

func (s *ConfigStore) PersistMCPDisabledOverride(scope Scope, name string, disabled bool) error {
	_, err := s.PersistMCPDisabledOverrideResult(scope, name, disabled)
	return err
}

func (s *ConfigStore) PersistMCPDisabledOverrideResult(scope Scope, name string, disabled bool) (MCPMutationResult, error) {
	return s.mutateMCP("disable", scope, name, name, MCPConfig{}, &disabled)
}

// PersistMCPDisabledOverrideExact changes only the selected scope. It may
// update a shadowed literal entry and may create a workspace overlay for an
// external server, but it never redirects the write to another scope.
func (s *ConfigStore) PersistMCPDisabledOverrideExact(scope Scope, name string, disabled bool) error {
	_, err := s.PersistMCPDisabledOverrideExactResult(scope, name, disabled)
	return err
}

func (s *ConfigStore) PersistMCPDisabledOverrideExactResult(scope Scope, name string, disabled bool) (MCPMutationResult, error) {
	return s.mutateMCPExact("disable", scope, name, name, MCPConfig{}, &disabled)
}

// PersistMCPDisabledOverrideAtScope changes only the selected scope.
func (s *ConfigStore) PersistMCPDisabledOverrideAtScope(scope Scope, name string, disabled bool) error {
	return s.PersistMCPDisabledOverrideExact(scope, name, disabled)
}

// PersistMCPDisabledOverrideInScope changes only the selected scope.
func (s *ConfigStore) PersistMCPDisabledOverrideInScope(scope Scope, name string, disabled bool) error {
	return s.PersistMCPDisabledOverrideExact(scope, name, disabled)
}

func (s *ConfigStore) PersistReplaceMCP(oldName, newName string, value MCPConfig) error {
	_, err := s.PersistReplaceMCPResult(ScopeGlobal, oldName, newName, value)
	return err
}

func (s *ConfigStore) PersistReplaceMCPResult(scope Scope, oldName, newName string, value MCPConfig) (MCPMutationResult, error) {
	return s.mutateMCP("replace", scope, oldName, newName, value, nil)
}

func (s *ConfigStore) PersistReplaceMCPInScope(scope Scope, oldName, newName string, value MCPConfig) error {
	_, err := s.PersistReplaceMCPResult(scope, oldName, newName, value)
	return err
}

func (s *ConfigStore) PersistReplaceMCPAtScope(scope Scope, oldName, newName string, value MCPConfig) error {
	return s.PersistReplaceMCPInScope(scope, oldName, newName, value)
}

// PersistMCPFields is the low-level literal-key field editor retained for
// non-lifecycle callers. It still takes the same ordered two-file transaction
// locks as lifecycle mutations.
func (s *ConfigStore) PersistMCPFields(scope Scope, name string, fields map[string]any) error {
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}
	path = normalizeReloadPath(path)
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var effective MCPConfig
	var effectiveExists bool
	s.publishMu.Lock()
	err = s.withMCPWriteLocks(func(files *mcpLockedFiles) error {
		before, evalErr := s.evaluateMCPFiles(files)
		if evalErr != nil {
			return evalErr
		}
		_, oldOK := before.configs[name]
		if err := validateMCPMutation("set", scope, name, name, oldOK, before.origins[name], before.configs); err != nil {
			return err
		}
		if err := updateMCPFile(files, path, name, func(entry map[string]any) error {
			for _, key := range keys {
				entry[key] = fields[key]
			}
			return nil
		}); err != nil {
			return err
		}
		if err := s.verifyMCPReadOnlyInputs(before.fingerprints, path); err != nil {
			return err
		}
		after, err := s.evaluateMCPFiles(files)
		if err != nil {
			return err
		}
		effectiveExists, effective, _ = afterValue(after, name)
		return writeMCPFileChanges(files)
	})
	if err == nil {
		// The selected file may be shadowed, so publish the effective value
		// only after the exact file mutation has committed. A later reload
		// remains authoritative for all other fields.
		s.publishMCPValueLocked(name, effective, effectiveExists)
	}
	s.publishMu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

// PersistMCPFieldsExact updates only the literal entry in the selected file.
// It does not resolve or validate the effective owner in another scope.
func (s *ConfigStore) PersistMCPFieldsExact(scope Scope, name string, fields map[string]any) error {
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}
	path = normalizeReloadPath(path)
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var effective MCPConfig
	var effectiveExists bool
	s.publishMu.Lock()
	err = s.withMCPWriteLocks(func(files *mcpLockedFiles) error {
		before, evalErr := s.evaluateMCPFiles(files)
		if evalErr != nil {
			return evalErr
		}
		if !literalMCPEntryExists(files.data[path], name) {
			return fmt.Errorf("%w: %q", ErrMCPNotFound, name)
		}
		if err := updateMCPFile(files, path, name, func(entry map[string]any) error {
			for _, key := range keys {
				entry[key] = fields[key]
			}
			return nil
		}); err != nil {
			return err
		}
		if err := s.verifyMCPReadOnlyInputs(before.fingerprints, path); err != nil {
			return err
		}
		after, err := s.evaluateMCPFiles(files)
		if err != nil {
			return err
		}
		effectiveExists, effective, _ = afterValue(after, name)
		return writeMCPFileChanges(files)
	})
	if err == nil {
		s.publishMCPValueLocked(name, effective, effectiveExists)
	}
	s.publishMu.Unlock()
	return err
}

// AtScope aliases make the exact-file intent explicit to callers that prefer
// the older lifecycle method naming convention.
func (s *ConfigStore) PersistMCPConfigAtScope(scope Scope, name string, value MCPConfig) error {
	return s.PersistMCPConfigExact(scope, name, value)
}

func (s *ConfigStore) PersistRemoveMCPConfigAtScope(scope Scope, name string) error {
	return s.PersistRemoveMCPConfigExact(scope, name)
}

func (s *ConfigStore) PersistMCPFieldsAtScope(scope Scope, name string, fields map[string]any) error {
	return s.PersistMCPFieldsExact(scope, name, fields)
}

func (s *ConfigStore) PersistMCPConfigInScope(scope Scope, name string, value MCPConfig) error {
	return s.PersistMCPConfigExact(scope, name, value)
}

func (s *ConfigStore) PersistRemoveMCPConfigInScope(scope Scope, name string) error {
	return s.PersistRemoveMCPConfigExact(scope, name)
}

func (s *ConfigStore) PersistMCPFieldsInScope(scope Scope, name string, fields map[string]any) error {
	return s.PersistMCPFieldsExact(scope, name, fields)
}

func (s *ConfigStore) publishMCPConfigLocked(oldName, newName string) {
	cur := s.loadSnapshot()
	if cur.config == nil {
		return
	}
	next := cur.clone()
	cfg := *cur.config
	cfg.MCP = maps.Clone(cur.config.MCP)
	if cfg.MCP == nil {
		cfg.MCP = make(MCPs)
	}
	if oldName != newName {
		delete(cfg.MCP, oldName)
	}
	next.config = &cfg
	s.publishLocked(next)
}

func (s *ConfigStore) publishMCPValueLocked(name string, value MCPConfig, exists bool) {
	cur := s.loadSnapshot()
	if cur.config == nil {
		return
	}
	next := cur.clone()
	cfg := *cur.config
	cfg.MCP = maps.Clone(cur.config.MCP)
	if cfg.MCP == nil {
		cfg.MCP = make(MCPs)
	}
	if exists {
		cfg.MCP[name] = cloneMCPConfig(value)
	} else {
		delete(cfg.MCP, name)
	}
	next.config = &cfg
	s.publishLocked(next)
}
