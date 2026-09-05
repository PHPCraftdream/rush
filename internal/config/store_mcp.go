package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

var (
	// ErrMCPNotFound indicates that no effective MCP server has the requested
	// literal name.
	ErrMCPNotFound = errors.New("MCP server not found")
	// ErrMCPExternal indicates that the effective server comes from .mcp.json.
	ErrMCPExternal = errors.New("MCP server is from .mcp.json and is not writable")
	// ErrMCPAmbiguous is kept as a semantic alias for callers that describe an
	// external origin as ambiguous rather than non-writable.
	ErrMCPAmbiguous = ErrMCPExternal
	// ErrMCPUnwritableOrigin indicates that a project or system config owns the
	// effective definition and neither writable Scope can represent it.
	ErrMCPUnwritableOrigin = errors.New("MCP server has no writable config scope")
)

// PersistMCPConfig atomically upserts one literal MCP server key in a config
// file. The server name is a JSON object key, not a sjson path, so dots,
// backslashes, wildcards, and every other supported key character are kept
// verbatim.
func (s *ConfigStore) PersistMCPConfig(scope Scope, name string, value MCPConfig) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("failed to encode MCP server %q: %w", name, err)
	}
	return s.persistMCPRaw(scope, func(servers map[string]json.RawMessage) error {
		servers[name] = raw
		return nil
	})
}

// PersistRemoveMCPConfig atomically removes one literal MCP server key from a
// config file.
func (s *ConfigStore) PersistRemoveMCPConfig(scope Scope, name string) error {
	return s.persistMCPRaw(scope, func(servers map[string]json.RawMessage) error {
		delete(servers, name)
		return nil
	})
}

// PersistMCPDisabledOverride atomically writes a disabled override for one
// literal MCP server key. This is used for servers supplied by .mcp.json.
func (s *ConfigStore) PersistMCPDisabledOverride(scope Scope, name string, disabled bool) error {
	return s.PersistMCPFields(scope, name, map[string]any{"disabled": disabled})
}

// ResolveMCPWritableScope returns the writable scope that owns the effective
// literal MCP definition. A workspace definition wins over a global one.
// External and project definitions fail closed because silently writing the
// global data file would change a different server definition.
func (s *ConfigStore) ResolveMCPWritableScope(name string) (Scope, error) {
	current, exists := s.MCPConfig(name)
	if !exists {
		return ScopeGlobal, fmt.Errorf("%w: %q", ErrMCPNotFound, name)
	}
	if current.Source == MCPSourceExternal {
		return ScopeGlobal, fmt.Errorf("%w: %q", ErrMCPExternal, name)
	}

	workspacePath, _ := s.configPath(ScopeWorkspace)
	globalPath, _ := s.configPath(ScopeGlobal)
	paths := s.LoadedPaths()
	seen := make(map[string]struct{}, len(paths))
	var owner string
	for _, path := range paths {
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return ScopeGlobal, fmt.Errorf("failed to read MCP origin %q: %w", name, err)
		}
		entry, ok := mcpEntryFromJSON(data, name)
		if ok && (path != filepath.Clean(workspacePath) && path != filepath.Clean(globalPath) || !isMCPDisabledOnlyEntry(entry, true)) {
			owner = path
		}
	}
	// The computed workspace file is merged after lookupConfigs and therefore
	// has the highest priority even when it was not in LoadedPaths.
	if workspacePath != "" {
		if data, err := os.ReadFile(workspacePath); err == nil {
			entry, ok := mcpEntryFromJSON(data, name)
			if ok && !isMCPDisabledOnlyEntry(entry, true) {
				owner = filepath.Clean(workspacePath)
			}
		} else if !os.IsNotExist(err) {
			return ScopeGlobal, fmt.Errorf("failed to read MCP origin %q: %w", name, err)
		}
	}
	if owner == "" && globalPath != "" {
		if data, err := os.ReadFile(globalPath); err == nil {
			entry, ok := mcpEntryFromJSON(data, name)
			if ok && !isMCPDisabledOnlyEntry(entry, true) {
				owner = filepath.Clean(globalPath)
			}
		} else if !os.IsNotExist(err) {
			return ScopeGlobal, fmt.Errorf("failed to read MCP origin %q: %w", name, err)
		}
	}

	switch owner {
	case filepath.Clean(workspacePath):
		return ScopeWorkspace, nil
	case filepath.Clean(globalPath):
		return ScopeGlobal, nil
	case "":
		return ScopeGlobal, fmt.Errorf("%w: %q has no on-disk writable definition", ErrMCPUnwritableOrigin, name)
	default:
		return ScopeGlobal, fmt.Errorf("%w: %q is defined in %s", ErrMCPUnwritableOrigin, name, owner)
	}
}

// PersistMCPFields atomically updates fields within one literal MCP server
// key. Both the server name and field names are treated as JSON object keys;
// neither is interpreted as gjson/sjson path syntax.
func (s *ConfigStore) PersistMCPFields(scope Scope, name string, fields map[string]any) error {
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	return s.persistMCPRawAt(path, func(servers map[string]json.RawMessage) error {
		entry := make(map[string]json.RawMessage)
		if raw := servers[name]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &entry); err != nil {
				return fmt.Errorf("failed to parse MCP server %q: %w", name, err)
			}
		}
		if entry == nil {
			entry = make(map[string]json.RawMessage)
		}
		for _, key := range keys {
			raw, err := json.Marshal(fields[key])
			if err != nil {
				return fmt.Errorf("failed to encode MCP field %q: %w", key, err)
			}
			entry[key] = raw
		}
		raw, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("failed to encode MCP server %q: %w", name, err)
		}
		servers[name] = raw
		return nil
	})
}

// PersistReplaceMCP atomically removes oldName and sets newName in the
// global MCP map, then publishes the matching copy-on-write snapshot. Disk is
// committed before the snapshot changes, and the operation has no fallible
// work after publication.
func (s *ConfigStore) PersistReplaceMCP(oldName, newName string, value MCPConfig) error {
	return s.PersistReplaceMCPInScope(ScopeGlobal, oldName, newName, value)
}

// PersistReplaceMCPInScope atomically removes oldName and sets newName in the
// selected writable MCP map, then publishes a matching copy-on-write
// snapshot. It is groundwork for callers that have already resolved the
// effective MCP origin; runtime replacement wiring remains responsible for
// choosing when to call it.
func (s *ConfigStore) PersistReplaceMCPInScope(scope Scope, oldName, newName string, value MCPConfig) error {
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}

	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if err := s.persistMCPRawAt(path, func(servers map[string]json.RawMessage) error {
		delete(servers, oldName)
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("failed to encode MCP server %q: %w", newName, err)
		}
		servers[newName] = raw
		return nil
	}); err != nil {
		return err
	}

	cur := s.loadSnapshot()
	next := cur.clone()
	if cur.config != nil {
		cfgCopy := *cur.config
		cfgCopy.MCP = maps.Clone(cur.config.MCP)
		if cfgCopy.MCP == nil {
			cfgCopy.MCP = make(MCPs)
		}
		delete(cfgCopy.MCP, oldName)
		if oldName != newName {
			if fallback, ok := s.mcpFallbackAfterReplace(scope, oldName); ok {
				cfgCopy.MCP[oldName] = fallback
			}
		}
		cfgCopy.MCP[newName] = cloneMCPConfig(value)
		next.config = &cfgCopy
	}
	s.publishLocked(next)
	return nil
}

func (s *ConfigStore) mcpFallbackAfterReplace(scope Scope, name string) (MCPConfig, bool) {
	otherScope := ScopeGlobal
	if scope == ScopeGlobal {
		otherScope = ScopeWorkspace
	}
	if path, err := s.configPath(otherScope); err == nil {
		if fallback, ok := readMCPConfigAtPath(path, name); ok {
			return fallback, true
		}
	}

	// A global full definition can reveal an external server after it is
	// removed. Preserve the same external connection details and overlay that
	// the normal load path would apply.
	external := loadExternalMCPServers(s.workingDir)
	fallback, ok := external[name]
	if !ok {
		return MCPConfig{}, false
	}
	if disabled, ok := readMCPDisabledOverride(s, ScopeWorkspace, name); ok {
		fallback.Disabled = disabled
	} else if disabled, ok := readMCPDisabledOverride(s, ScopeGlobal, name); ok {
		fallback.Disabled = disabled
	}
	return fallback, true
}

func readMCPConfigAtPath(path, name string) (MCPConfig, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return MCPConfig{}, false
	}
	entry, ok := mcpEntryFromJSON(data, name)
	if !ok || isMCPDisabledOnlyEntry(entry, true) {
		return MCPConfig{}, false
	}
	entryData, err := json.Marshal(entry)
	if err != nil {
		return MCPConfig{}, false
	}
	var fallback MCPConfig
	if json.Unmarshal(entryData, &fallback) != nil {
		return MCPConfig{}, false
	}
	return fallback, true
}

// PersistReplaceMCPAtScope is an explicit alias for the scoped replacement
// API. It keeps the target scope prominent at call sites.
func (s *ConfigStore) PersistReplaceMCPAtScope(scope Scope, oldName, newName string, value MCPConfig) error {
	return s.PersistReplaceMCPInScope(scope, oldName, newName, value)
}

func (s *ConfigStore) persistMCPRaw(scope Scope, mutate func(map[string]json.RawMessage) error) error {
	path, err := s.configPath(scope)
	if err != nil {
		return err
	}
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	return s.persistMCPRawAt(path, mutate)
}

func (s *ConfigStore) persistMCPRawAt(path string, mutate func(map[string]json.RawMessage) error) error {
	return s.withConfigWriteLock(path, func() error {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				data = []byte("{}")
			} else {
				return fmt.Errorf("failed to read config file: %w", err)
			}
		}

		var root map[string]json.RawMessage
		if err := json.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("failed to parse config file: %w", err)
		}
		if root == nil {
			root = make(map[string]json.RawMessage)
		}
		servers := make(map[string]json.RawMessage)
		if raw := root["mcp"]; len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &servers); err != nil {
				return fmt.Errorf("failed to parse MCP config: %w", err)
			}
		}
		if servers == nil {
			servers = make(map[string]json.RawMessage)
		}
		if err := mutate(servers); err != nil {
			return err
		}
		raw, err := json.Marshal(servers)
		if err != nil {
			return fmt.Errorf("failed to encode MCP config: %w", err)
		}
		root["mcp"] = raw
		newValue, err := json.MarshalIndent(root, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to encode config file: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("failed to create config directory %q: %w", path, err)
		}
		if err := atomicWriteFile(path, append(newValue, '\n'), 0o600); err != nil {
			return fmt.Errorf("failed to write config file: %w", err)
		}
		return nil
	})
}
