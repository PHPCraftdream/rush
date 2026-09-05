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

func (s *ConfigStore) PersistMCPConfigResult(scope Scope, name string, value MCPConfig) (MCPMutationResult, error) {
	return s.mutateMCP("add", scope, name, name, value, nil)
}

func (s *ConfigStore) PersistRemoveMCPConfig(scope Scope, name string) error {
	_, err := s.PersistRemoveMCPConfigResult(scope, name)
	return err
}

func (s *ConfigStore) PersistRemoveMCPConfigResult(scope Scope, name string) (MCPMutationResult, error) {
	return s.mutateMCP("remove", scope, name, name, MCPConfig{}, nil)
}

func (s *ConfigStore) PersistMCPDisabledOverride(scope Scope, name string, disabled bool) error {
	_, err := s.PersistMCPDisabledOverrideResult(scope, name, disabled)
	return err
}

func (s *ConfigStore) PersistMCPDisabledOverrideResult(scope Scope, name string, disabled bool) (MCPMutationResult, error) {
	return s.mutateMCP("disable", scope, name, name, MCPConfig{}, &disabled)
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
	s.publishMu.Lock()
	err = s.withMCPWriteLocks(func(files *mcpLockedFiles) error {
		return updateMCPFile(files, path, name, func(entry map[string]any) error {
			for _, key := range keys {
				entry[key] = fields[key]
			}
			return nil
		})
	})
	if err == nil {
		s.publishMCPConfigLocked(name, name)
	}
	s.publishMu.Unlock()
	if err != nil {
		return err
	}
	return nil
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
