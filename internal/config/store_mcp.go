package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	path, err := s.configPath(ScopeGlobal)
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
		cfgCopy.MCP[newName] = cloneMCPConfig(value)
		next.config = &cfgCopy
	}
	s.publishLocked(next)
	return nil
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
